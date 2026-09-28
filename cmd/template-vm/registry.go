package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// lowerLayer is one remote overlaybd layer, addressed by digest+size, that
// becomes an entry of config.v1.json lowers[] (glossary: lower / 只读层).
type lowerLayer struct {
	Digest string
	Size   int64
}

// resolvedImage is the outcome of pulling an overlaybd image manifest: the
// ordered (bottom-up) lower layers and the repoBlobUrl overlaybd fetches
// blocks from at runtime.
type resolvedImage struct {
	Lowers      []lowerLayer
	RepoBlobURL string
}

// dockerConfig models the docker config json carried in template.json's
// dockerAuth. It matches overlaybd's cred.json auths shape, so the same
// object is reused for both the ORAS client credentials and the credential
// handoff into overlaybd's cred.json.
type dockerConfig struct {
	Auths map[string]authEntry `json:"auths"`
}

type authEntry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// parseDockerConfig decodes the dockerAuth string. An empty string yields an
// empty (anonymous) config.
func parseDockerConfig(dockerAuth string) (dockerConfig, error) {
	if strings.TrimSpace(dockerAuth) == "" {
		return dockerConfig{Auths: map[string]authEntry{}}, nil
	}
	var config dockerConfig
	if err := json.Unmarshal([]byte(dockerAuth), &config); err != nil {
		return dockerConfig{}, fmt.Errorf("parse dockerAuth: %w", err)
	}
	if config.Auths == nil {
		config.Auths = map[string]authEntry{}
	}
	return config, nil
}

// normalizeAuthKey strips the scheme and trailing slash from a docker config
// registry key so it can be matched against a bare host/repository.
func normalizeAuthKey(key string) string {
	key = strings.TrimSuffix(key, "/")
	if index := strings.Index(key, "://"); index >= 0 {
		key = key[index+3:]
	}
	return key
}

func parseUsernamePassword(value string) (authEntry, error) {
	username, password, found := strings.Cut(value, ":")
	if !found || strings.TrimSpace(username) == "" {
		return authEntry{}, fmt.Errorf("--username must be username:password")
	}
	return authEntry{Username: username, Password: password}, nil
}
