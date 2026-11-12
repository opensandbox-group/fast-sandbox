package reconciler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"fast-sandbox/internal/registryconfig"
)

// compileRegistryConfig reads the fast-sandbox-registry ConfigMap in namespace
// and compiles it into the registry.json payload the agents consume (pull
// credentials resolved from each rule's dockerconfigjson Secret, plus any
// artifact-store write key). An absent ConfigMap compiles to an empty set, so
// callers get a valid (empty) Compiled rather than an error — a namespace with
// no registry rules simply has no private credentials.
//
// Both the SandboxPool reconciler (for fastlet mounts) and the SandboxTemplate
// reconciler (for build-Pod mounts) share this single implementation so the two
// produce byte-identical compiled output for the same inputs; the pool rollout
// keys off the compiled Revision, so the shape must not drift between callers.
func compileRegistryConfig(ctx context.Context, c client.Client, namespace string) (registryconfig.Compiled, error) {
	var source corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: registryconfig.ConfigMapName}, &source)
	if apierrors.IsNotFound(err) {
		return registryconfig.NewCompiled(nil)
	}
	if err != nil {
		return registryconfig.Compiled{}, fmt.Errorf("read Registry ConfigMap: %w", err)
	}
	raw, found := source.Data[registryconfig.ConfigMapKey]
	if !found {
		return registryconfig.Compiled{}, fmt.Errorf("configMap %s must contain %s", registryconfig.ConfigMapName, registryconfig.ConfigMapKey)
	}
	var config registryconfig.Config
	if err := yaml.UnmarshalStrict([]byte(raw), &config); err != nil {
		return registryconfig.Compiled{}, fmt.Errorf("decode Registry configuration: %w", err)
	}
	config, err = registryconfig.NormalizeAndValidate(config)
	if err != nil {
		return registryconfig.Compiled{}, err
	}
	credentials := make([]registryconfig.Credential, 0, len(config.Registries))
	for _, rule := range config.Registries {
		credential, err := registryCredentialFromSecret(ctx, c, namespace, rule)
		if err != nil {
			return registryconfig.Compiled{}, err
		}
		credentials = append(credentials, credential)
	}
	return registryconfig.NewCompiled(credentials)
}

// registryCredentialFromSecret resolves one registry rule's pull credential
// from its kubernetes.io/dockerconfigjson Secret, matching the rule host
// against the Secret's auths map (username/password or base64 auth), and
// attaches the optional artifact-store write key pair.
func registryCredentialFromSecret(
	ctx context.Context,
	c client.Client,
	namespace string,
	rule registryconfig.RegistryRule,
) (registryconfig.Credential, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: rule.SecretRef.Name}, &secret); err != nil {
		return registryconfig.Credential{}, fmt.Errorf("read Registry Secret %s: %w", rule.SecretRef.Name, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return registryconfig.Credential{}, fmt.Errorf("registry Secret %s must have type %s", secret.Name, corev1.SecretTypeDockerConfigJson)
	}
	content := secret.Data[corev1.DockerConfigJsonKey]
	if len(content) == 0 {
		return registryconfig.Credential{}, fmt.Errorf("registry Secret %s has no %s data", secret.Name, corev1.DockerConfigJsonKey)
	}
	var dockerConfig dockerConfigJSON
	if err := json.Unmarshal(content, &dockerConfig); err != nil {
		return registryconfig.Credential{}, fmt.Errorf("decode Registry Secret %s: %w", secret.Name, err)
	}
	var auth dockerAuthConfig
	found := false
	for host, candidate := range dockerConfig.Auths {
		if registryconfig.NormalizeHost(host) == rule.Host {
			auth = candidate
			found = true
			break
		}
	}
	if !found {
		return registryconfig.Credential{}, fmt.Errorf("registry Secret %s has no credentials for host %s", secret.Name, rule.Host)
	}
	if auth.Username == "" && auth.Password == "" && auth.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err != nil {
			return registryconfig.Credential{}, fmt.Errorf("decode auth for host %s in Secret %s: %w", rule.Host, secret.Name, err)
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return registryconfig.Credential{}, fmt.Errorf("auth for host %s in Secret %s is invalid", rule.Host, secret.Name)
		}
		auth.Username, auth.Password = username, password
	}
	if auth.Username == "" && auth.Password == "" && auth.IdentityToken == "" {
		return registryconfig.Credential{}, fmt.Errorf("registry Secret %s has empty credentials for host %s", secret.Name, rule.Host)
	}
	credential := registryconfig.Credential{
		Host: rule.Host, RepositoryPrefix: rule.RepositoryPrefix,
		Username: auth.Username, Password: auth.Password, IdentityToken: auth.IdentityToken,
	}
	if rule.WriteSecretRef != nil {
		writeUsername, writePassword, err := registryWriteCredential(ctx, c, namespace, rule.Host, *rule.WriteSecretRef)
		if err != nil {
			return registryconfig.Credential{}, err
		}
		credential.WriteUsername, credential.WritePassword = writeUsername, writePassword
	}
	return credential, nil
}

// registryWriteCredential reads the Opaque write secret of an artifact-store
// rule (accessKeyId/secretAccessKey keys) and validates the pair is complete.
func registryWriteCredential(ctx context.Context, c client.Client, namespace, host string, ref registryconfig.SecretRef) (string, string, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", "", fmt.Errorf("read write Secret %s for host %s: %w", ref.Name, host, err)
	}
	username := strings.TrimSpace(string(secret.Data[writeSecretAccessKeyID]))
	password := strings.TrimSpace(string(secret.Data[writeSecretAccessKey]))
	if username == "" || password == "" {
		return "", "", fmt.Errorf("write Secret %s for host %s must contain non-empty %s and %s", secret.Name, host, writeSecretAccessKeyID, writeSecretAccessKey)
	}
	return username, password, nil
}
