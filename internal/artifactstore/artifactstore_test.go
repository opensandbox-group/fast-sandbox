package artifactstore

import (
	"os"
	"path/filepath"
	"testing"
)

func writeKey(t *testing.T, dir, key, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key), []byte(value), 0o644); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
}

func TestLoaderReadsProjectedKeys(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, StoreKey, "s3://bucket/prefix\n")
	writeKey(t, dir, EndpointKey, " http://minio:9000 \n")

	config, err := Loader{Dir: dir}.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Store != "s3://bucket/prefix" || config.Endpoint != "http://minio:9000" {
		t.Fatalf("expected trimmed values, got %+v", config)
	}
}

func TestLoaderMissingEndpointIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, StoreKey, "s3://bucket/prefix")

	config, err := Loader{Dir: dir}.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Store != "s3://bucket/prefix" || config.Endpoint != "" {
		t.Fatalf("expected an absent endpoint to read as empty, got %+v", config)
	}
}

func TestLoaderMissingMountIsUnconfigured(t *testing.T) {
	config, err := Loader{Dir: filepath.Join(t.TempDir(), "absent")}.Load()
	if err != nil {
		t.Fatalf("load missing mount: %v", err)
	}
	if config != (Config{}) {
		t.Fatalf("expected an unconfigured store, got %+v", config)
	}
}

func TestLoaderObservesFileChanges(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, StoreKey, "s3://bucket/first")
	loader := Loader{Dir: dir}

	first, err := loader.Load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	writeKey(t, dir, StoreKey, "s3://bucket/second")
	second, err := loader.Load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first.Store != "s3://bucket/first" || second.Store != "s3://bucket/second" {
		t.Fatalf("expected the file change to be observed, got %q then %q", first.Store, second.Store)
	}
}
