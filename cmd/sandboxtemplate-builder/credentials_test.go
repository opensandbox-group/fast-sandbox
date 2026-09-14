package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCredentialFile(t *testing.T, dir, key, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key), []byte(value), 0o600); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
}

func TestLoadPublishCredentialsReadsMountedKeys(t *testing.T) {
	dir := t.TempDir()
	writeCredentialFile(t, dir, publishSecretKeyID, "AKID\n")
	writeCredentialFile(t, dir, publishSecretKeySecret, " SKEY ")
	writeCredentialFile(t, dir, publishSecretKeyPoint, "http://minio:9000")
	writeCredentialFile(t, dir, publishSecretKeyRegion, "cn-hangzhou\n")

	credentials, err := loadPublishCredentials(dir)
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	if credentials.AccessKeyID != "AKID" || credentials.SecretAccessKey != "SKEY" ||
		credentials.Endpoint != "http://minio:9000" || credentials.Region != "cn-hangzhou" {
		t.Fatalf("expected trimmed mounted values, got %+v", credentials)
	}
}

func TestLoadPublishCredentialsWithoutMountIsAmbient(t *testing.T) {
	credentials, err := loadPublishCredentials("")
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	if credentials != (publishCredentials{}) {
		t.Fatalf("expected empty credentials without a mount, got %+v", credentials)
	}
}

func TestLoadPublishCredentialsIncompleteMountFails(t *testing.T) {
	dir := t.TempDir()
	writeCredentialFile(t, dir, publishSecretKeyID, "AKID")

	_, err := loadPublishCredentials(dir)
	if err == nil || !strings.Contains(err.Error(), publishSecretKeySecret) {
		t.Fatalf("expected a missing-key error naming %s, got %v", publishSecretKeySecret, err)
	}
}

func TestPublishCredentialsAwsEnvOverridesAmbient(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("AWS_REGION", "ambient-region")

	env := publishCredentials{
		AccessKeyID: "mounted", SecretAccessKey: "mounted-secret", Region: "mounted-region",
	}.awsEnv()

	for key, want := range map[string]string{
		"AWS_ACCESS_KEY_ID":     "mounted",
		"AWS_SECRET_ACCESS_KEY": "mounted-secret",
		"AWS_REGION":            "mounted-region",
	} {
		count := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				count++
				if entry != key+"="+want {
					t.Fatalf("expected %s=%s, got %q", key, want, entry)
				}
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly one %s entry, got %d (%v)", key, count, env)
		}
	}
}

func TestPublishCredentialsAwsEnvKeepsEnvironment(t *testing.T) {
	t.Setenv("CHAIN_MARKER", "kept")
	env := publishCredentials{}.awsEnv()
	found := false
	for _, entry := range env {
		if entry == "CHAIN_MARKER=kept" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the process environment to be preserved, got %v", env)
	}
}
