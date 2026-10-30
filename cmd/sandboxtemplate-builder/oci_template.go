package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/ocitemplate"
)

const buildIDEnvName = "SANDBOX_TEMPLATE_BUILD_ID"

// buildOCITemplate delegates disk conversion, cold boot and snapshots to fsb-vm.
// It shares the existing guest bootstrap contract with native templates.
func buildOCITemplate(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir, kernel string) error {
	if spec.Output.Format != apiv1alpha2.ArtifactFormatOverlayBD {
		return fmt.Errorf("OCI templates require output.format=overlaybd")
	}
	if os.Getenv("SANDBOX_TEMPLATE_IMAGE_TAR") != "" {
		return fmt.Errorf("SANDBOX_TEMPLATE_IMAGE_TAR is supported only for object-store output; OCI templates require a registry source")
	}
	destination, err := ociBuildDestination(spec.Output.Publish, os.Getenv(buildIDEnvName))
	if err != nil {
		return err
	}
	authDir := os.Getenv(publishCredentialsDirEnv)
	authFile := filepath.Join(authDir, ".dockerconfigjson")
	if _, err := os.Stat(authFile); err != nil {
		return fmt.Errorf("OCI publishSecretRef must contain .dockerconfigjson: %w", err)
	}
	dockerDir := filepath.Join(workdir, "docker-auth")
	if err := os.MkdirAll(dockerDir, 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(authFile)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dockerDir, "config.json"), data, 0600); err != nil {
		return err
	}
	if err = os.Setenv("DOCKER_CONFIG", dockerDir); err != nil {
		return err
	}
	if err = stageOCICredentials(spec, dockerDir); err != nil {
		return err
	}
	source, sourceDigest, err := prepareOCISource(ctx, spec, workdir)
	if err != nil {
		return err
	}
	inject := filepath.Join(workdir, "inject")
	if err = os.MkdirAll(filepath.Join(inject, "etc"), 0755); err != nil {
		return err
	}
	ca, err := injectRuntime(spec, workdir, inject)
	if err != nil {
		return err
	}
	vcpu, err := resource.ParseQuantity(spec.Machine.VCPU)
	if err != nil {
		return err
	}
	memory, err := resource.ParseQuantity(spec.Machine.Memory)
	if err != nil {
		return err
	}
	sizeQuantity, err := resource.ParseQuantity(spec.Output.RootfsSize)
	if err != nil || sizeQuantity.Sign() <= 0 {
		return fmt.Errorf("rootfsSize must be a positive resource quantity")
	}
	size := artifacts.SizeGiB(sizeQuantity.Value())

	tap := buildTap
	if err = ensureBuildTap(); err != nil {
		return err
	}
	defer deleteBuildTap()
	args := []string{"--firecracker", firecrackerBin, "--overlaybd-convertor", "/usr/local/bin/convertor", "--build-work-dir", filepath.Join(workdir, "builds"), "--build-output-dir", filepath.Join(workdir, "artifacts"), "build", "--source", source, "--output", "template:build", "--kernel", kernel, "--kernel-version", strings.TrimPrefix(filepath.Base(kernel), "vmlinux-"), "--vcpu", strconv.FormatInt(vcpu.Value(), 10), "--memory-mb", strconv.FormatInt((memory.Value()+(1<<20)-1)/(1<<20), 10), "--disk-size-gb", strconv.Itoa(size), "--inject-dir", inject, "--init-path", guestInitPath(spec), "--init-script-file", filepath.Join(inject, strings.TrimPrefix(guestInitPath(spec), "/")), "--boot-args", bakedNetworkBootArgs("console=ttyS0 reboot=k panic=1 pci=off nomodules net.ifnames=0 biosdevname=0 random.trust_cpu=on root=/dev/vda rw init=" + guestInitPath(spec)), "--tap-name", tap, "--guest-mac", bakedGuestMAC, "--ready-pattern", "SANDBOX_READY", "--ready-timeout", readinessTimeout(spec).String(), "--auth-file", authFile}
	platform := templatePlatform(spec, ca)
	extra := struct {
		Lineage      *artifacts.VMLineage  `json:"lineage"`
		GuestNetwork map[string]any        `json:"guestNetwork"`
		Platform     *artifacts.VMPlatform `json:"platform"`
	}{Lineage: &artifacts.VMLineage{Image: spec.Image, ImageDigest: sourceDigest},
		GuestNetwork: map[string]any{"ip": bakedGuestIP, "gateway": bakedGuestGateway, "netmask": bakedGuestNetmask, "mtu": bakedGuestMTU}, Platform: platform}
	raw, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	extraFile := filepath.Join(workdir, "manifest-extra.json")
	if err = os.WriteFile(extraFile, raw, 0600); err != nil {
		return err
	}
	args = append(args, "--manifest-extra-file", extraFile)
	cmd := exec.CommandContext(ctx, "fsb-vm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("fsb-vm build: %w", err)
	}
	cmd = exec.CommandContext(ctx, "fsb-vm", "push", "--artifact-dir", filepath.Join(workdir, "artifacts", "template_build"), "--destination", destination, "--auth-file", authFile)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("fsb-vm push: %w", err)
	}
	var result struct {
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
	}
	if err = json.Unmarshal(output, &result); err != nil {
		return err
	}
	if result.ManifestRef == "" || result.ArtifactDigest == "" {
		return fmt.Errorf("fsb-vm push returned incomplete publication")
	}
	return patchPodAnnotations(ctx, result.ManifestRef, result.ArtifactDigest)
}

// The controller pins buildID in the Pod. Local builder runs get a fresh UUID.
func ociBuildDestination(repository, buildID string) (string, error) {
	if err := ocitemplate.ValidatePublishRepository(repository); err != nil {
		return "", err
	}
	if buildID == "" {
		buildID = uuid.NewString()
	}
	id, err := uuid.Parse(buildID)
	if err != nil {
		return "", fmt.Errorf("invalid OCI build UUID: %w", err)
	}
	return strings.TrimPrefix(repository, ocitemplate.Prefix) + ":" + id.String(), nil
}

// prepareOCISource reads only the source metadata. fsb-vm downloads the layers
// directly from this pinned platform reference instead of a staged OCI layout.
func prepareOCISource(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir string) (string, string, error) {
	ref, err := name.ParseReference(spec.Image)
	if err != nil {
		return "", "", fmt.Errorf("parse image %q: %w", spec.Image, err)
	}
	descriptor, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(linuxAMD64))
	if err != nil {
		return "", "", fmt.Errorf("read source manifest %q: %w", spec.Image, err)
	}
	image, err := descriptor.Image()
	if err != nil {
		return "", "", err
	}
	sum, err := image.Digest()
	if err != nil {
		return "", "", err
	}
	if err := writeImageEnv(image, workdir); err != nil {
		return "", "", err
	}
	if spec.Execd != "" {
		execdRef, err := name.ParseReference(spec.Execd)
		if err != nil {
			return "", "", fmt.Errorf("parse execd image %q: %w", spec.Execd, err)
		}
		execdAuth, err := authn.DefaultKeychain.Resolve(execdRef.Context())
		if err != nil {
			return "", "", fmt.Errorf("resolve execd credentials %q: %w", spec.Execd, err)
		}
		if err := extractExecd(ctx, spec.Execd, filepath.Join(workdir, "execd-root"), execdAuth); err != nil {
			return "", "", err
		}
	}
	return ref.Context().Digest(sum.String()).Name(), sum.String(), nil
}

// Keep the builder's private Docker config on the same basic-only contract as
// fsb-vm. go-containerregistry otherwise prefers an identity token over basic auth.
func stageOCICredentials(spec apiv1alpha2.SandboxTemplateSpec, dockerDir string) error {
	config := struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}{Auths: map[string]authn.AuthConfig{}}
	for _, value := range []string{spec.Image, spec.Execd, strings.TrimPrefix(spec.Output.Publish, ocitemplate.Prefix)} {
		if value == "" {
			continue
		}
		ref, err := name.ParseReference(value)
		if err != nil {
			return err
		}
		authenticator, err := authn.DefaultKeychain.Resolve(ref.Context())
		if err != nil {
			return err
		}
		credential, err := authenticator.Authorization()
		if err != nil {
			return err
		}
		basic, err := ocitemplate.BasicCredential(credential.Username, credential.Password, credential.IdentityToken)
		if err != nil {
			return fmt.Errorf("OCI credentials for %s: %w", ref.Context(), err)
		}
		if basic.Username == "" && basic.Password == "" {
			continue
		}
		config.Auths[ref.Context().String()] = authn.AuthConfig{Username: basic.Username, Password: basic.Password}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dockerDir, "config.json"), raw, 0600)
}
