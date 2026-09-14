package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Publish-secret keys (imagePullSecrets-style, per the design doc).
const (
	publishSecretKeyID     = "accessKeyId"
	publishSecretKeySecret = "secretAccessKey"
	publishSecretKeyRegion = "region"
	publishSecretKeyPoint  = "endpoint"
)

// publishCredentials is the object-store credential set of the build, mounted
// from the template's publishSecretRef Secret: the controller mounts the
// Secret into the build Pod and points the builder at the mount through
// SANDBOX_TEMPLATE_PUBLISH_SECRET_DIR. Empty fields fall back to the ambient
// AWS environment (IRSA, node metadata, or an explicit pair as the chain E2E
// exports).
type publishCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string
	Region          string
}

// loadPublishCredentials reads the mounted Secret keys as files. A configured
// directory that is incomplete fails the build: a missing key must not
// degrade to anonymous access (the old env injection failed at container
// start for the same reason). An empty dir means no publishSecretRef.
func loadPublishCredentials(dir string) (publishCredentials, error) {
	if dir == "" {
		return publishCredentials{}, nil
	}
	credentials := publishCredentials{}
	for _, entry := range []struct {
		key    string
		target *string
	}{
		{publishSecretKeyID, &credentials.AccessKeyID},
		{publishSecretKeySecret, &credentials.SecretAccessKey},
		{publishSecretKeyPoint, &credentials.Endpoint},
		{publishSecretKeyRegion, &credentials.Region},
	} {
		payload, err := os.ReadFile(filepath.Join(dir, entry.key))
		if err != nil {
			return publishCredentials{}, fmt.Errorf("read publish credential %s from %s: %w", entry.key, dir, err)
		}
		*entry.target = strings.TrimSpace(string(payload))
	}
	return credentials, nil
}

// awsEnv builds the subprocess environment for the aws CLI: the process
// environment plus the mounted credential pair. The mount wins over an
// exported value so a stale ambient key cannot shadow the Secret.
func (c publishCredentials) awsEnv() []string {
	env := setEnv(os.Environ(), "AWS_ACCESS_KEY_ID", c.AccessKeyID)
	env = setEnv(env, "AWS_SECRET_ACCESS_KEY", c.SecretAccessKey)
	env = setEnv(env, "AWS_REGION", c.Region)
	return env
}

func setEnv(env []string, key, value string) []string {
	if value == "" {
		return env
	}
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
