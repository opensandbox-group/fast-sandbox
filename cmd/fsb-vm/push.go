package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/registry/remote/auth"

	"fast-sandbox/internal/ocitemplate"
)

func newPushCommand() *cobra.Command {
	var dir, destination, metadata, authFile string
	cmd := &cobra.Command{Use: "push", Short: "Publish an OverlayBD artifact to an OCI registry", RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := os.ReadFile(authFile)
		if err != nil {
			return err
		}
		cfg, err := parseDockerConfig(string(raw))
		if err != nil {
			return err
		}
		if _, err = parseRegistryReference(destination); err != nil {
			return err
		}
		payload := []byte(`{}`)
		if metadata != "" {
			payload, err = os.ReadFile(metadata)
			if err != nil {
				return err
			}
		}
		manifestRef, digest, err := ocitemplate.Push(cmd.Context(), dir, destination, payload, func(_ context.Context, reference string) (auth.Credential, error) {
			ref, err := parseRegistryReference(reference)
			if err != nil {
				return auth.EmptyCredential, err
			}
			credential, _, err := credentialFor(cfg, ref)
			return credential, err
		}, ocitemplate.WithPlainHTTP(global.plainHTTP))
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"manifestRef": manifestRef, "artifactDigest": digest})
	}}
	f := cmd.Flags()
	f.StringVar(&dir, "artifact-dir", "", "local build artifact directory")
	f.StringVar(&destination, "destination", "", "registry/repository:tag")
	f.StringVar(&metadata, "metadata-file", "", "optional manifest.json used only to identify checkpoint ownership")
	f.StringVar(&authFile, "auth-file", "", "Docker authentication JSON")
	for _, name := range []string{"artifact-dir", "destination", "auth-file"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprint(err))
		}
	}
	return cmd
}
