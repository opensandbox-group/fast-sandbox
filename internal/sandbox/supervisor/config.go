package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
)

const ConfigVersion = 1

type Config struct {
	Version        int             `json:"version"`
	SandboxUID     string          `json:"sandboxUid"`
	UserCredential *UserCredential `json:"-"`
	Components     []Component     `json:"components,omitempty"`
}

// UserCredential is populated by the runtime wrapper, not persisted in the
// instance file. sandbox-init stays privileged enough to read platform-only
// configuration and drops to the image's original OCI user for the user
// entrypoint.
type UserCredential struct {
	UID            uint32
	GID            uint32
	AdditionalGIDs []uint32
}

type Component struct {
	Name            string                     `json:"name"`
	Command         string                     `json:"command"`
	Args            []string                   `json:"args,omitempty"`
	Env             map[string]string          `json:"env,omitempty"`
	StartBeforeUser bool                       `json:"startBeforeUser,omitempty"`
	RestartPolicy   infracatalog.RestartPolicy `json:"restartPolicy"`
	Readiness       Readiness                  `json:"readiness"`
	Required        bool                       `json:"required"`
	DependsOn       []string                   `json:"dependsOn,omitempty"`
}

type Readiness struct {
	Type     infracatalog.ProbeType `json:"type"`
	Address  string                 `json:"address,omitempty"`
	Path     string                 `json:"path,omitempty"`
	Timeout  time.Duration          `json:"timeout,omitempty"`
	Interval time.Duration          `json:"interval,omitempty"`
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.Version != ConfigVersion || c.SandboxUID == "" {
		return fmt.Errorf("sandbox-init config version and Sandbox UID are required")
	}
	seen := make(map[string]struct{}, len(c.Components))
	for _, component := range c.Components {
		if component.Name == "" || component.Command == "" {
			return fmt.Errorf("component name and command are required")
		}
		if _, exists := seen[component.Name]; exists {
			return fmt.Errorf("duplicate component %q", component.Name)
		}
		seen[component.Name] = struct{}{}
	}
	for _, component := range c.Components {
		for _, dependency := range component.DependsOn {
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("component %s depends on unknown component %s", component.Name, dependency)
			}
		}
	}
	if _, err := orderedComponents(c.Components); err != nil {
		return err
	}
	return nil
}
