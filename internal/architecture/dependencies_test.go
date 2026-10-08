package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInternalDependencyBoundaries(t *testing.T) {
	root := repositoryRoot(t)
	tests := []struct {
		name      string
		directory string
		forbidden []string
	}{
		{
			name:      "data-plane contract is implementation neutral",
			directory: "internal/dataplane/contract",
			forbidden: []string{"fast-sandbox/internal/fastlet", "fast-sandbox/internal/runtime"},
		},
		{
			name:      "runtime contract is implementation neutral",
			directory: "internal/runtime/contract",
			forbidden: []string{"fast-sandbox/internal/fastlet", "fast-sandbox/internal/runtime/containerd"},
		},
		{
			name:      "Sandbox-side components do not import Fastlet implementation",
			directory: "internal/sandbox",
			forbidden: []string{"fast-sandbox/internal/fastlet"},
		},
		{
			name:      "Sandbox Proxy does not depend on Fastlet Proxy",
			directory: "internal/dataplane/sandboxproxy",
			forbidden: []string{"fast-sandbox/internal/dataplane/fastletproxy"},
		},
		{
			name:      "Fastlet Proxy does not depend on Fastlet implementation",
			directory: "internal/dataplane/fastletproxy",
			forbidden: []string{"fast-sandbox/internal/fastlet"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertNoForbiddenImports(t, filepath.Join(root, test.directory), test.forbidden)
		})
	}
}

func TestPrivateRuntimeMarkersStayInExtensionBoundary(t *testing.T) {
	root := repositoryRoot(t)
	markers := []string{
		"RuntimeRunD",
		"io.containerd.rund.v2",
		"alibabacloud.com/rund-enabled",
		"/home/admin/t4",
	}
	allowedPrefixes := []string{
		"api/v1alpha2/",
		"config/crd/",
		"config/runtime-environments/rund",
		"internal/architecture/dependencies_test.go",
		"internal/catalog/runtime/rund",
		"test/e2e/runtimes/rund/",
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == ".tools" || entry.Name() == "bin" || entry.Name() == ".build" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, prefix := range allowedPrefixes {
			if strings.HasPrefix(filepath.ToSlash(relative), prefix) {
				return nil
			}
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range markers {
			if strings.Contains(string(contents), marker) {
				t.Errorf("private runtime marker %q leaked into %s", marker, relative)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan repository: %v", err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve dependency test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func assertNoForbiddenImports(t *testing.T, directory string, forbidden []string) {
	t.Helper()
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, prefix := range forbidden {
				if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
					position := file.Pos()
					if spec.Pos().IsValid() {
						position = spec.Pos()
					}
					t.Errorf("%s imports forbidden package %q at position %d", path, importPath, position)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", directory, err)
	}
}
