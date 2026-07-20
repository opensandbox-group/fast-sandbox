package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const legacyMaxSandboxesPerPod int32 = 5

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Convert legacy Fast Sandbox manifests to the canonical API",
}

func init() {
	rootCmd.AddCommand(migrateCmd)
	migrateCmd.AddCommand(newMigratePoolCommand())
}

func newMigratePoolCommand() *cobra.Command {
	var inputPath string
	var outputPath string
	var check bool
	command := &cobra.Command{
		Use:   "pool",
		Short: "Convert a SandboxPool manifest to runtime/resources/infraProfile",
	}
	command.RunE = func(_ *cobra.Command, _ []string) error {
		if inputPath == "" {
			return errors.New("--file is required")
		}
		input, err := os.ReadFile(inputPath)
		if err != nil {
			return fmt.Errorf("read Pool manifest: %w", err)
		}
		output, changed, err := migratePoolManifest(input)
		if err != nil {
			return err
		}
		if check {
			if changed {
				return errors.New("SandboxPool manifest requires migration")
			}
			return nil
		}
		if outputPath == "" || outputPath == "-" {
			_, err = command.OutOrStdout().Write(output)
			return err
		}
		if err := os.WriteFile(outputPath, output, 0644); err != nil {
			return fmt.Errorf("write migrated Pool manifest: %w", err)
		}
		return nil
	}
	command.Flags().StringVarP(&inputPath, "file", "f", "", "SandboxPool YAML manifest to read")
	command.Flags().StringVarP(&outputPath, "output", "o", "-", "Output path, or - for stdout")
	command.Flags().BoolVar(&check, "check", false, "Exit non-zero when the manifest still requires migration")
	return command
}

func migratePoolManifest(input []byte) ([]byte, bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(input))
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	changed := false
	documents := 0
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("decode Pool manifest: %w", err)
		}
		if len(document) == 0 {
			continue
		}
		documentChanged, err := migratePoolDocument(document)
		if err != nil {
			return nil, false, fmt.Errorf("document %d: %w", documents+1, err)
		}
		changed = changed || documentChanged
		if err := encoder.Encode(document); err != nil {
			return nil, false, fmt.Errorf("encode migrated Pool manifest: %w", err)
		}
		documents++
	}
	if documents == 0 {
		return nil, false, errors.New("Pool manifest contains no YAML documents")
	}
	if err := encoder.Close(); err != nil {
		return nil, false, fmt.Errorf("finish migrated Pool manifest: %w", err)
	}
	return output.Bytes(), changed, nil
}

func migratePoolDocument(document map[string]any) (bool, error) {
	if document["apiVersion"] != apiv1alpha1.GroupVersion.String() || document["kind"] != "SandboxPool" {
		return false, fmt.Errorf("expected %s SandboxPool manifest", apiv1alpha1.GroupVersion.String())
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return false, fmt.Errorf("normalize Pool manifest: %w", err)
	}
	var pool apiv1alpha1.SandboxPool
	if err := json.Unmarshal(encoded, &pool); err != nil {
		return false, fmt.Errorf("decode SandboxPool fields: %w", err)
	}
	runtimeName, err := pool.Spec.EffectiveRuntime()
	if err != nil {
		return false, fmt.Errorf("resolve Pool runtime: %w", err)
	}
	resources, err := pool.Spec.EffectiveSandboxResources()
	if err != nil {
		return false, fmt.Errorf("resolve Pool sandboxResources: %w", err)
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return false, errors.New("SandboxPool spec must be a YAML mapping")
	}
	changed := false
	if value, _ := spec["runtime"].(string); value != string(runtimeName) {
		spec["runtime"] = string(runtimeName)
		changed = true
	}
	for _, field := range []string{"runtimeType", "runtimeClassName", "containerdRuntimeHandler"} {
		if _, exists := spec[field]; exists {
			delete(spec, field)
			changed = true
		}
	}
	if _, exists := spec["sandboxResources"]; !exists {
		spec["sandboxResources"] = map[string]any{
			"cpu": resources.CPU.String(), "memory": resources.Memory.String(), "pids": resources.PIDs,
		}
		changed = true
	}
	if pool.Spec.MaxSandboxesPerPod == 0 {
		spec["maxSandboxesPerPod"] = legacyMaxSandboxesPerPod
		changed = true
	}
	if pool.Spec.InfraProfile == "" {
		spec["infraProfile"] = "minimal"
		changed = true
	}
	return changed, nil
}
