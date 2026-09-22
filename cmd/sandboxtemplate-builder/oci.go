package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// linuxAMD64 is the fixed target platform of the build (oci2rootfs converts
// with --platform linux/amd64).
var linuxAMD64 = v1.Platform{OS: "linux", Architecture: "amd64"}

// imageEnvFileName is the workdir file where the pull stage persists the
// source image's OCI Config.Env (the merged Dockerfile ENV). The convert
// stage merges it into the guest /etc/sandbox-init.env and the manifest
// stage records it under lineage.imageEnvs.
const imageEnvFileName = "image-config.env"

// writeImageEnv persists the source image's OCI Config.Env for the convert
// and manifest stages. An image without config envs produces an empty file:
// readers treat missing and empty alike.
func writeImageEnv(image v1.Image, workdir string) error {
	config, err := image.ConfigFile()
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, imageEnvFileName),
		[]byte(strings.Join(config.Config.Env, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", imageEnvFileName, err)
	}
	return nil
}

// pullOCILayout pulls spec.Image into an OCI layout directory under workdir,
// ready for oci2rootfs, and returns the image manifest digest. When
// SANDBOX_TEMPLATE_IMAGE_TAR is set, the layout is produced from a local
// Docker-save style tarball instead of pulling from a registry (E2E and
// offline builds). The tarball must contain a single image (a docker save of
// one image with any number of tags is fine; multi-image archives are not
// supported and an arbitrary member would be picked).
func pullOCILayout(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir string) (string, error) {
	if tarPath := os.Getenv("SANDBOX_TEMPLATE_IMAGE_TAR"); tarPath != "" {
		return pullOCILayoutFromTar(ctx, spec, workdir, tarPath)
	}
	reference, err := name.ParseReference(spec.Image)
	if err != nil {
		return "", fmt.Errorf("parse image %q: %w", spec.Image, err)
	}
	// Pin the platform explicitly: the builder converts for linux/amd64, and
	// without a platform the registry's multi-arch index resolves against the
	// builder host (an arm64 host would pull the wrong architecture).
	descriptor, err := remote.Get(reference, remote.WithContext(ctx), remote.WithPlatform(linuxAMD64))
	if err != nil {
		return "", fmt.Errorf("pull %q: %w", spec.Image, err)
	}
	image, err := descriptor.Image()
	if err != nil {
		return "", err
	}
	if err := writeOCILayout(image, workdir); err != nil {
		return "", err
	}
	if err := writeImageEnv(image, workdir); err != nil {
		return "", err
	}
	if spec.Execd != "" {
		if err := extractExecd(ctx, spec.Execd, filepath.Join(workdir, "execd-root")); err != nil {
			return "", err
		}
	}
	return descriptor.Digest.String(), nil
}

func pullOCILayoutFromTar(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir, tarPath string) (string, error) {
	image, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		return "", fmt.Errorf("load image tarball: %w", err)
	}
	if err := writeOCILayout(image, workdir); err != nil {
		return "", err
	}
	if err := writeImageEnv(image, workdir); err != nil {
		return "", err
	}
	if spec.Execd != "" {
		if err := extractExecd(ctx, spec.Execd, filepath.Join(workdir, "execd-root")); err != nil {
			return "", err
		}
	}
	digest, err := image.Digest()
	if err != nil {
		return "", err
	}
	return digest.String(), nil
}

// writeOCILayout stores the image as a single-entry index in workdir/oci-layout.
func writeOCILayout(image v1.Image, workdir string) error {
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image})
	layoutDir := filepath.Join(workdir, "oci-layout")
	if err := os.RemoveAll(layoutDir); err != nil {
		return err
	}
	if _, err := layout.Write(layoutDir, index); err != nil {
		return fmt.Errorf("write OCI layout: %w", err)
	}
	return nil
}

// extractExecd pulls the execd image and copies its runtime files
// (/execd, /bootstrap.sh, /prepare.sh, /usr/local/bin/bwrap) into a local
// directory for injection. Files are first collected in memory so symlinked
// entries (e.g. bwrap -> execd) can be resolved across the layer.
func extractExecd(ctx context.Context, image, destination string) error { //nolint:gocognit // single-pass tar walk; splitting would obscure symlink resolution
	reference, err := name.ParseReference(image)
	if err != nil {
		return err
	}
	img, err := remote.Image(reference, remote.WithContext(ctx), remote.WithPlatform(linuxAMD64))
	if err != nil {
		return fmt.Errorf("pull execd %q: %w", image, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	want := map[string]string{
		execdAssetName:        execdAssetName,
		bootstrapScriptName:   bootstrapScriptName,
		prepareScriptName:     prepareScriptName,
		"usr/local/bin/bwrap": "bwrap",
	}
	files := map[string][]byte{}
	symlinks := map[string]string{}
	for _, layer := range layers {
		reader, err := layer.Uncompressed()
		if err != nil {
			return err
		}
		tarReader := tar.NewReader(reader)
		for {
			header, err := tarReader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = reader.Close()
				return err
			}
			name := strings.TrimPrefix(header.Name, "./")
			if header.Typeflag == tar.TypeSymlink {
				if _, ok := want[name]; ok {
					symlinks[name] = header.Linkname
				}
				continue
			}
			if header.Typeflag != tar.TypeReg {
				continue
			}
			target, ok := want[name]
			if !ok {
				continue
			}
			payload, err := io.ReadAll(tarReader)
			if err != nil {
				_ = reader.Close()
				return err
			}
			files[target] = payload
		}
		if err := reader.Close(); err != nil {
			return err
		}
	}
	// Resolve symlinks against the collected files (link targets inside the
	// image are names from `want`).
	for name, target := range symlinks {
		linkedTarget, ok := want[strings.TrimPrefix(target, "./")]
		if !ok {
			klog.V(2).InfoS("execd symlink target not extracted, skipping", "link", name, "target", target)
			continue
		}
		payload, ok := files[linkedTarget]
		if !ok {
			klog.V(2).InfoS("execd symlink target file missing, skipping", "link", name, "target", target)
			continue
		}
		files[want[name]] = payload
	}
	for name, payload := range files {
		if err := extractFile(bytes.NewReader(payload), filepath.Join(destination, name)); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(source io.Reader, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, 0o755)
}
