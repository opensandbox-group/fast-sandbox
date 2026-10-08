package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// templateFile is the sole CLI input manifest (glossary: template.json).
type templateFile struct {
	SandboxID string          `json:"sandbox_id"`
	Template  templateRef     `json:"template"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// templateRef identifies the remote template: repo + template_id name the
// image pair, and auth carries the registry credential.
type templateRef struct {
	Repo       string       `json:"repo"`
	TemplateID string       `json:"template_id"`
	Auth       templateAuth `json:"auth"`
}

// templateAuth holds the registry credential. dockerAuth is a docker config
// json string ({"auths":{"<host>":{"auth":"base64(user:pass)"}}}); it is used
// both to authenticate the manifest pulls and, merged into overlaybd's global
// cred.json, to authorize overlaybd's on-demand block reads (ADR-003).
type templateAuth struct {
	DockerAuth string `json:"dockerAuth"`
}

// rootfsRef returns the overlaybd rootfs image reference
// (${repo}:${template_id}_rootfs).
func (t templateFile) rootfsRef() string {
	return fmt.Sprintf("%s:%s_rootfs", t.Template.Repo, t.Template.TemplateID)
}

// snapfilesRef returns the overlaybd snapfiles image reference
// (${repo}:${template_id}_snapfiles).
func (t templateFile) snapfilesRef() string {
	return fmt.Sprintf("%s:%s_snapfiles", t.Template.Repo, t.Template.TemplateID)
}

// loadTemplate reads and validates template.json.
func loadTemplate(path string) (templateFile, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return templateFile{}, fmt.Errorf("read template %s: %w", path, err)
	}
	var template templateFile
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&template); err != nil {
		// Retry leniently: the metadata passthrough field is reserved and
		// future producers may add keys the CLI does not model yet.
		if err := json.Unmarshal(payload, &template); err != nil {
			return templateFile{}, fmt.Errorf("parse template %s: %w", path, err)
		}
	}
	if strings.TrimSpace(template.SandboxID) == "" {
		return templateFile{}, fmt.Errorf("template %s: sandbox_id is required", path)
	}
	if strings.TrimSpace(template.Template.Repo) == "" {
		return templateFile{}, fmt.Errorf("template %s: template.repo is required", path)
	}
	if strings.TrimSpace(template.Template.TemplateID) == "" {
		return templateFile{}, fmt.Errorf("template %s: template.template_id is required", path)
	}
	return template, nil
}
