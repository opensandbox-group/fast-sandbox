package registryconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	ConfigMapName = "fast-sandbox-registry"
	ConfigMapKey  = "registries.yaml"
	SecretKey     = "registry.json"
	MountPath     = "/etc/fast-sandbox/registry/registry.json"
)

type Config struct {
	Registries []RegistryRule `json:"registries,omitempty" yaml:"registries,omitempty"`
}

type RegistryRule struct {
	Host             string    `json:"host" yaml:"host"`
	RepositoryPrefix string    `json:"repositoryPrefix,omitempty" yaml:"repositoryPrefix,omitempty"`
	SecretRef        SecretRef `json:"secretRef" yaml:"secretRef"`
	// WriteSecretRef optionally references an Opaque secret holding the
	// publish (write) access key pair of an S3-compatible artifact store:
	// keys accessKeyId and secretAccessKey (the SandboxTemplate
	// PublishSecretRef convention). Empty keeps the store read-only.
	WriteSecretRef *SecretRef `json:"writeSecretRef,omitempty" yaml:"writeSecretRef,omitempty"`
}

type SecretRef struct {
	Name string `json:"name" yaml:"name"`
}

type Credential struct {
	Host             string `json:"host"`
	RepositoryPrefix string `json:"repositoryPrefix,omitempty"`
	Username         string `json:"username,omitempty"`
	Password         string `json:"password,omitempty"`
	IdentityToken    string `json:"identityToken,omitempty"`
	// WriteUsername/WritePassword carry the optional publish (write) access
	// key pair of an S3-compatible artifact store. Empty keeps the store
	// read-only: pulls work, the runtime-agent refuses to publish. The
	// fields are compiled from a RegistryRule's writeSecretRef (plain
	// accessKeyId/secretAccessKey keys, mirroring the SandboxTemplate
	// PublishSecretRef convention).
	WriteUsername string `json:"writeUsername,omitempty"`
	WritePassword string `json:"writePassword,omitempty"`
	// Endpoint is the connection address of an S3-compatible artifact store
	// (firecracker runtime-agent), e.g. "http://127.0.0.1:9000". Host keeps
	// its matching-key meaning (registry host, or the artifact store host
	// matched against the store root); Endpoint may carry the scheme and
	// port and is used verbatim, so it is exempt from host normalization.
	// Image-registry credentials leave it empty.
	Endpoint string `json:"endpoint,omitempty"`
}

type Compiled struct {
	Revision    string       `json:"revision"`
	Credentials []Credential `json:"credentials,omitempty"`
}

type Provider interface {
	Credentials(reference string) (Credential, bool, error)
	Revision() string
}

func NormalizeAndValidate(config Config) (Config, error) {
	result := Config{Registries: make([]RegistryRule, 0, len(config.Registries))}
	seen := make(map[string]struct{}, len(config.Registries))
	for _, rule := range config.Registries {
		rule.Host = NormalizeHost(rule.Host)
		rule.RepositoryPrefix = strings.Trim(strings.TrimSpace(rule.RepositoryPrefix), "/")
		rule.SecretRef.Name = strings.TrimSpace(rule.SecretRef.Name)
		if rule.WriteSecretRef != nil {
			rule.WriteSecretRef.Name = strings.TrimSpace(rule.WriteSecretRef.Name)
			if rule.WriteSecretRef.Name == "" {
				return Config{}, fmt.Errorf("registry %s has an empty writeSecretRef.name", rule.Host)
			}
		}
		if rule.Host == "" {
			return Config{}, errors.New("registry host is required")
		}
		if strings.ContainsAny(rule.Host, "/ \t\r\n") {
			return Config{}, fmt.Errorf("registry host %q is invalid", rule.Host)
		}
		if rule.SecretRef.Name == "" {
			return Config{}, fmt.Errorf("registry %s requires secretRef.name", rule.Host)
		}
		key := rule.Host + "/" + rule.RepositoryPrefix
		if _, exists := seen[key]; exists {
			return Config{}, fmt.Errorf("duplicate registry match %q", key)
		}
		seen[key] = struct{}{}
		result.Registries = append(result.Registries, rule)
	}
	sort.Slice(result.Registries, func(i, j int) bool {
		if result.Registries[i].Host != result.Registries[j].Host {
			return result.Registries[i].Host < result.Registries[j].Host
		}
		return result.Registries[i].RepositoryPrefix < result.Registries[j].RepositoryPrefix
	})
	return result, nil
}

func NewCompiled(credentials []Credential) (Compiled, error) {
	normalized := append([]Credential(nil), credentials...)
	for index := range normalized {
		normalized[index].Host = NormalizeHost(normalized[index].Host)
		normalized[index].RepositoryPrefix = strings.Trim(normalized[index].RepositoryPrefix, "/")
		if normalized[index].Host == "" {
			return Compiled{}, errors.New("compiled registry credential host is required")
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Host != normalized[j].Host {
			return normalized[i].Host < normalized[j].Host
		}
		return normalized[i].RepositoryPrefix < normalized[j].RepositoryPrefix
	})
	payload, err := json.Marshal(normalized)
	if err != nil {
		return Compiled{}, err
	}
	digest := sha256.Sum256(payload)
	return Compiled{Revision: "sha256:" + hex.EncodeToString(digest[:]), Credentials: normalized}, nil
}

func (c Compiled) Marshal() ([]byte, error) {
	return json.Marshal(c)
}

func ParseCompiled(content []byte) (Compiled, error) {
	var compiled Compiled
	if err := json.Unmarshal(content, &compiled); err != nil {
		return Compiled{}, fmt.Errorf("decode compiled registry configuration: %w", err)
	}
	expected, err := NewCompiled(compiled.Credentials)
	if err != nil {
		return Compiled{}, err
	}
	if compiled.Revision != expected.Revision {
		return Compiled{}, fmt.Errorf("compiled registry revision %q does not match content %q", compiled.Revision, expected.Revision)
	}
	return compiled, nil
}

func LoadCompiled(path string) (Compiled, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Compiled{}, err
	}
	return ParseCompiled(content)
}

func (c Compiled) Match(reference string) (Credential, bool) {
	host, repository := splitReference(reference)
	var selected Credential
	selectedLength := -1
	for _, credential := range c.Credentials {
		if NormalizeHost(credential.Host) != host {
			continue
		}
		prefix := strings.Trim(credential.RepositoryPrefix, "/")
		if prefix != "" && repository != prefix && !strings.HasPrefix(repository, prefix+"/") {
			continue
		}
		if len(prefix) > selectedLength {
			selected = credential
			selectedLength = len(prefix)
		}
	}
	return selected, selectedLength >= 0
}

// MatchHost selects the credential whose normalized Host equals host,
// ignoring repository prefixes. This is the artifact-store lookup of the
// firecracker runtime-agent: the match key is the store endpoint host
// itself (e.g. "127.0.0.1:9000"), not an image reference, so the
// reference-splitting rules of Match do not apply.
func (c Compiled) MatchHost(host string) (Credential, bool) {
	normalized := NormalizeHost(host)
	for _, credential := range c.Credentials {
		if NormalizeHost(credential.Host) == normalized {
			return credential, true
		}
	}
	return Credential{}, false
}

type FileProvider struct {
	path        string
	mu          sync.RWMutex
	contentHash [sha256.Size]byte
	loaded      bool
	compiled    Compiled
}

func NewFileProvider(path string) *FileProvider {
	return &FileProvider{path: path}
}

func (p *FileProvider) Credentials(reference string) (Credential, bool, error) {
	compiled, err := p.load()
	if err != nil {
		return Credential{}, false, err
	}
	credential, found := compiled.Match(reference)
	return credential, found, nil
}

// CredentialsForHost resolves a credential by normalized host match (the
// artifact-store lookup of the firecracker runtime-agent), bypassing the
// image-reference semantics of Match.
func (p *FileProvider) CredentialsForHost(host string) (Credential, bool, error) {
	compiled, err := p.load()
	if err != nil {
		return Credential{}, false, err
	}
	credential, found := compiled.MatchHost(host)
	return credential, found, nil
}

func (p *FileProvider) Revision() string {
	compiled, err := p.load()
	if err != nil {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.compiled.Revision
	}
	return compiled.Revision
}

func (p *FileProvider) Refresh() (string, error) {
	compiled, err := p.load()
	if err != nil {
		return "", err
	}
	return compiled.Revision, nil
}

func (p *FileProvider) load() (Compiled, error) {
	content, err := os.ReadFile(p.path)
	if err != nil {
		return Compiled{}, err
	}
	contentHash := sha256.Sum256(content)
	p.mu.RLock()
	if p.loaded && p.contentHash == contentHash {
		result := p.compiled
		p.mu.RUnlock()
		return result, nil
	}
	p.mu.RUnlock()

	compiled, err := ParseCompiled(content)
	if err != nil {
		return Compiled{}, err
	}
	p.mu.Lock()
	p.loaded = true
	p.contentHash = contentHash
	p.compiled = compiled
	p.mu.Unlock()
	return compiled, nil
}

func NormalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	host = strings.TrimSuffix(host, "/v1")
	switch host {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	default:
		return host
	}
}

func splitReference(reference string) (string, string) {
	value := strings.TrimSpace(strings.ToLower(reference))
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.SplitN(value, "@", 2)[0]
	parts := strings.Split(value, "/")
	host := "docker.io"
	repositoryParts := parts
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		host = NormalizeHost(parts[0])
		repositoryParts = parts[1:]
	}
	repository := strings.Join(repositoryParts, "/")
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	if host == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return host, strings.Trim(repository, "/")
}
