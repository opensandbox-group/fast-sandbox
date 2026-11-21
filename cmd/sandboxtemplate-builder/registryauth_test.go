package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	"fast-sandbox/internal/registryconfig"
)

// writeCompiledRegistry writes a valid compiled registry.json (revision matches
// content) built from the given credentials and returns its path.
func writeCompiledRegistry(t *testing.T, credentials []registryconfig.Credential) string {
	t.Helper()
	compiled, err := registryconfig.NewCompiled(credentials)
	if err != nil {
		t.Fatalf("compile registry: %v", err)
	}
	payload, err := compiled.Marshal()
	if err != nil {
		t.Fatalf("marshal compiled: %v", err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write registry.json: %v", err)
	}
	return path
}

func authConfig(t *testing.T, a authn.Authenticator) authn.AuthConfig {
	t.Helper()
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	return *cfg
}

func TestLoadRegistryAuthEmptyPathIsAnonymous(t *testing.T) {
	auth, err := loadRegistryAuth("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := auth.authenticator("jd-registry.example.com/system:python_3.12"); got != authn.Anonymous {
		t.Fatalf("expected anonymous without a mounted config, got %#v", got)
	}
}

func TestRegistryAuthHostMatchCarriesCredentials(t *testing.T) {
	path := writeCompiledRegistry(t, []registryconfig.Credential{
		{Host: "jd-registry.example.com", Username: "u", Password: "p"},
	})
	auth, err := loadRegistryAuth(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg := authConfig(t, auth.authenticator("jd-registry.example.com/system:python_3.12"))
	if cfg.Username != "u" || cfg.Password != "p" {
		t.Fatalf("expected matched credentials u/p, got %+v", cfg)
	}
}

func TestRegistryAuthLongestPrefixWins(t *testing.T) {
	path := writeCompiledRegistry(t, []registryconfig.Credential{
		{Host: "jd-registry.example.com", Username: "root", Password: "r"},
		{Host: "jd-registry.example.com", RepositoryPrefix: "system", Username: "sys", Password: "s"},
	})
	auth, err := loadRegistryAuth(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg := authConfig(t, auth.authenticator("jd-registry.example.com/system:python_3.12"))
	if cfg.Username != "sys" {
		t.Fatalf("expected longest-prefix credential sys, got %+v", cfg)
	}
}

func TestRegistryAuthNoMatchIsAnonymous(t *testing.T) {
	path := writeCompiledRegistry(t, []registryconfig.Credential{
		{Host: "jd-registry.example.com", Username: "u", Password: "p"},
	})
	auth, err := loadRegistryAuth(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := auth.authenticator("other-registry.example.com/foo:bar"); got != authn.Anonymous {
		t.Fatalf("expected anonymous for an unmatched host, got %#v", got)
	}
}

func TestRegistryAuthExecdSecondHostResolvesIndependently(t *testing.T) {
	path := writeCompiledRegistry(t, []registryconfig.Credential{
		{Host: "src-registry.example.com", Username: "src", Password: "1"},
		{Host: "execd-registry.example.com", Username: "exe", Password: "2"},
	})
	auth, err := loadRegistryAuth(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	src := authConfig(t, auth.authenticator("src-registry.example.com/base:latest"))
	exe := authConfig(t, auth.authenticator("execd-registry.example.com/execd:latest"))
	if src.Username != "src" || exe.Username != "exe" {
		t.Fatalf("expected per-host credentials, got src=%+v exe=%+v", src, exe)
	}
}

func TestLoadRegistryAuthRejectsGarbledFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadRegistryAuth(path); err == nil {
		t.Fatal("expected error for a garbled registry config file")
	}
}

func TestLoadRegistryAuthRejectsRevisionMismatch(t *testing.T) {
	// A payload whose revision does not match its content must fail loudly
	// (ParseCompiled recomputes and compares the revision).
	path := filepath.Join(t.TempDir(), "registry.json")
	bad := `{"revision":"sha256:deadbeef","credentials":[{"host":"jd-registry.example.com","username":"u","password":"p"}]}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadRegistryAuth(path); err == nil {
		t.Fatal("expected error for a revision mismatch")
	}
}
