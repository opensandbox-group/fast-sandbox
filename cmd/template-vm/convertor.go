package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const convertedSourceDirName = "converted-source"

type convertorRunner interface {
	Run(context.Context, string, []string) ([]byte, error)
}

type execConvertorRunner struct{}

func (execConvertorRunner) Run(ctx context.Context, binary string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, args...).CombinedOutput()
}

func convertorCredential(config dockerConfig, ref resolvedBuildSource) (string, error) {
	credential, found, err := credentialFor(config, ref.Ref)
	if err != nil {
		return "", err
	}
	if !found || credential == auth.EmptyCredential {
		return "", nil
	}
	return credential.Username + ":" + credential.Password, nil
}

func buildConvertorArgs(source resolvedBuildSource, outputDir string, targetGiB int, credential string, plainHTTP bool) []string {
	args := []string{
		"--repository", source.Ref.Registry + "/" + source.Ref.Repository,
		"--input-digest", source.ManifestDesc.Digest.String(),
		"--overlaybd", "template-vm-local",
		"--oci",
		"--mkfs=true",
		"--vsize", strconv.Itoa(targetGiB),
		"--no-upload",
		"--dump-manifest",
		"--reserve",
		"--dir", outputDir,
	}
	if credential != "" {
		args = append(args, "--username", credential)
	}
	if plainHTTP {
		args = append(args, "--plain")
	}
	return args
}

func runOCIConvertor(ctx context.Context, source resolvedBuildSource, outputDir string, targetGiB int, credential string, plainHTTP bool, runner convertorRunner) error {
	binary, err := exec.LookPath(global.convertor)
	if err != nil {
		return fmt.Errorf("find overlaybd convertor %q: %w", global.convertor, err)
	}
	output, err := runner.Run(ctx, binary, buildConvertorArgs(source, outputDir, targetGiB, credential, plainHTTP))
	if err != nil {
		return fmt.Errorf("convert OCI image to overlaybd: %w: %s", err, redactConvertorOutput(output, credential))
	}
	return nil
}

func redactConvertorOutput(output []byte, credential string) string {
	const limit = 64 << 10
	if len(output) > limit {
		output = output[len(output)-limit:]
	}
	text := string(bytes.TrimSpace(output))
	if credential == "" {
		return text
	}
	text = strings.ReplaceAll(text, credential, "<redacted>")
	if _, password, found := strings.Cut(credential, ":"); found && password != "" {
		text = strings.ReplaceAll(text, password, "<redacted>")
	}
	return text
}

func findConvertedWorkDir(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read convertor output %s: %w", root, err)
	}
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if info, err := os.Stat(filepath.Join(candidate, "manifest.json")); err == nil && !info.IsDir() {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("convertor output %s has %d platform manifests, want exactly 1", root, len(matches))
	}
	return matches[0], nil
}

func verifyDescriptorBytes(desc ocispec.Descriptor, payload []byte, label string) error {
	if err := validateSHA256Descriptor(desc, label); err != nil {
		return err
	}
	if int64(len(payload)) != desc.Size {
		return fmt.Errorf("%s size mismatch: got %d, want %d", label, len(payload), desc.Size)
	}
	if digest.FromBytes(payload) != desc.Digest {
		return fmt.Errorf("%s digest mismatch: got %s, want %s", label, digest.FromBytes(payload), desc.Digest)
	}
	return nil
}

func validateConvertedManifest(manifest ocispec.Manifest, imageRef string) error {
	if manifest.SchemaVersion != 2 {
		return fmt.Errorf("converted manifest schemaVersion is %d, want 2", manifest.SchemaVersion)
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return fmt.Errorf("converted manifest mediaType is %q, want %q", manifest.MediaType, ocispec.MediaTypeImageManifest)
	}
	if manifest.Config.MediaType != ocispec.MediaTypeImageConfig {
		return fmt.Errorf("converted config mediaType is %q, want %q", manifest.Config.MediaType, ocispec.MediaTypeImageConfig)
	}
	return validateManifestDescriptors(manifest, imageRef)
}

func validateConvertedConfig(payload []byte) error {
	var config ocispec.Image
	if err := json.Unmarshal(payload, &config); err != nil {
		return fmt.Errorf("parse converted config: %w", err)
	}
	if config.Architecture == "" || config.OS == "" {
		return fmt.Errorf("converted config must specify architecture and os")
	}
	return nil
}

func importConvertedImage(source resolvedBuildSource, convertorRoot string, layout artifactLayout) (fetchedImage, error) {
	workDir, err := findConvertedWorkDir(convertorRoot)
	if err != nil {
		return fetchedImage{}, err
	}
	manifestRaw, err := os.ReadFile(filepath.Join(workDir, "manifest.json"))
	if err != nil {
		return fetchedImage{}, fmt.Errorf("read converted manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fetchedImage{}, fmt.Errorf("parse converted manifest: %w", err)
	}
	if err := validateConvertedManifest(manifest, source.Ref.String()); err != nil {
		return fetchedImage{}, err
	}
	if len(manifest.Layers) != len(source.Manifest.Layers) {
		return fetchedImage{}, fmt.Errorf("converted manifest has %d layers, source has %d", len(manifest.Layers), len(source.Manifest.Layers))
	}
	if err := validateOverlayBDSource(manifest, source.Ref.String()); err != nil {
		return fetchedImage{}, fmt.Errorf("validate converted image: %w", err)
	}

	configRaw, err := os.ReadFile(filepath.Join(workDir, "config.json"))
	if err != nil {
		return fetchedImage{}, fmt.Errorf("read converted config: %w", err)
	}
	if err := verifyDescriptorBytes(manifest.Config, configRaw, "converted config"); err != nil {
		return fetchedImage{}, err
	}
	if err := validateConvertedConfig(configRaw); err != nil {
		return fetchedImage{}, err
	}
	if _, err := layout.installBytes(configRaw, manifest.Config.MediaType); err != nil {
		return fetchedImage{}, fmt.Errorf("install converted config: %w", err)
	}

	for index, desc := range manifest.Layers {
		sourceLayer := source.Manifest.Layers[index]
		if err := sourceLayer.Digest.Validate(); err != nil {
			return fetchedImage{}, fmt.Errorf("source layer %d digest: %w", index, err)
		}
		layerDir := fmt.Sprintf("%04d_%s", index, sourceLayer.Digest.String())
		layerPath := filepath.Join(workDir, layerDir, "overlaybd.commit")
		if err := layout.importVerifiedFile(layerPath, desc); err != nil {
			return fetchedImage{}, fmt.Errorf("install converted layer %d: %w", index, err)
		}
	}

	manifestDesc := ocispec.Descriptor{
		MediaType: manifest.MediaType,
		Digest:    digest.FromBytes(manifestRaw),
		Size:      int64(len(manifestRaw)),
	}
	manifestPath := layout.manifestPath(sourceManifestName(source.Ref.Reference))
	if err := writeFileAtomic(manifestPath, manifestRaw, 0o640); err != nil {
		return fetchedImage{}, fmt.Errorf("install converted manifest: %w", err)
	}
	return fetchedImage{
		SourceRef:    source.Ref.String(),
		ManifestDesc: manifestDesc,
		ManifestPath: manifestPath,
		Layers:       manifest.Layers,
		Layout:       layout,
	}, nil
}
