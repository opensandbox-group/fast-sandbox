// registryseed copies a source image into the local plain-HTTP registry via
// ORAS and optionally emits the compiled registry credential file:
//
//	go run ./test/e2e/support/registryseed \
//	  -from alpine:3.19 -to 127.0.0.1:5000/templates:base \
//	  -compiled-out /tmp/registry.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	registryconfig "fast-sandbox/internal/registryconfig"
)

func main() {
	var from, to, compiledOut string
	flag.StringVar(&from, "from", "", "source image reference (tag)")
	flag.StringVar(&to, "to", "", "destination image reference (tag, plain HTTP)")
	flag.StringVar(&compiledOut, "compiled-out", "", "optional path for the compiled registry credential JSON")
	flag.Parse()
	if from == "" || to == "" {
		fmt.Fprintln(os.Stderr, "usage: registryseed -from <ref> -to <ref> [-compiled-out <path>]")
		os.Exit(2)
	}
	ctx := context.Background()
	source, err := newRepository(from, false)
	if err != nil {
		fatal(err)
	}
	destination, err := newRepository(to, true)
	if err != nil {
		fatal(err)
	}
	sourceRef := parseOrFatal(from)
	destRef := parseOrFatal(to)
	descriptor, err := oras.Copy(ctx, source, sourceRef.Reference, destination, destRef.Reference, oras.CopyOptions{})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("seeded %s -> %s (digest %s, size %d)\n", from, to, descriptor.Digest, descriptor.Size)
	if compiledOut == "" {
		return
	}
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: destRef.Registry, RepositoryPrefix: destRef.Repository,
	}})
	if err != nil {
		fatal(err)
	}
	payload, err := compiled.Marshal()
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(compiledOut, payload, 0o600); err != nil {
		fatal(err)
	}
	fmt.Printf("compiled registry credentials written to %s\n", compiledOut)
}

func newRepository(reference string, plainHTTP bool) (*remote.Repository, error) {
	ref := parseOrFatal(reference)
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = plainHTTP
	repo.Client = &auth.Client{Cache: auth.NewCache()}
	return repo, nil
}

func parseOrFatal(reference string) registry.Reference {
	ref, err := registry.ParseReference(reference)
	if err != nil {
		fatal(fmt.Errorf("parse %s: %w", reference, err))
	}
	return ref
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "registryseed:", err)
	os.Exit(1)
}
