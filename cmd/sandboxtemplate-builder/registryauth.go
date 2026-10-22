package main

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"

	"fast-sandbox/internal/registryconfig"
)

// registryConfigPathEnv carries the compiled registry configuration file path
// into the build Pod. It matches the env the controller already sets for
// fastlets (FAST_SANDBOX_REGISTRY_CONFIG_PATH), so the controller can mount the
// same compiled registry.json for a build Pod.
const registryConfigPathEnv = "FAST_SANDBOX_REGISTRY_CONFIG_PATH"

// registryAuth resolves per-registry pull credentials for the source and execd
// image pulls. The build Pod pulls those images itself through
// go-containerregistry, which authenticates anonymously unless an
// authenticator is supplied; a private registry then answers 401. This maps a
// compiled registry configuration (the same shape the fastlet consumes) onto a
// go-containerregistry Authenticator so the builder can pull private images.
type registryAuth struct {
	compiled registryconfig.Compiled
	present  bool
}

// loadRegistryAuth reads the compiled registry configuration from path. An
// empty path yields an anonymous resolver with no error (local/E2E runs with no
// configuration mounted). A configured-but-unreadable or revision-mismatched
// file fails loudly, mirroring loadPublishCredentials: a broken mount must not
// silently degrade to anonymous and surface later as a confusing 401.
func loadRegistryAuth(path string) (*registryAuth, error) {
	if path == "" {
		return &registryAuth{}, nil
	}
	compiled, err := registryconfig.LoadCompiled(path)
	if err != nil {
		return nil, fmt.Errorf("load registry configuration from %s: %w", path, err)
	}
	return &registryAuth{compiled: compiled, present: true}, nil
}

// authenticator returns the Authenticator for an image reference, selecting the
// credential by longest host+repository-prefix match (Compiled.Match normalizes
// and splits the reference itself, so the raw spec.Image / spec.Execd value is
// passed verbatim). References with no matching rule — and every reference when
// no configuration was mounted — resolve to anonymous access, preserving public
// pulls.
func (a *registryAuth) authenticator(reference string) authn.Authenticator {
	if a == nil || !a.present {
		return authn.Anonymous
	}
	credential, found := a.compiled.Match(reference)
	if !found {
		return authn.Anonymous
	}
	return authn.FromConfig(authn.AuthConfig{
		Username:      credential.Username,
		Password:      credential.Password,
		IdentityToken: credential.IdentityToken,
	})
}
