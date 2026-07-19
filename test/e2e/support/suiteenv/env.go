package suiteenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/conf"
	envpkg "sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	DefaultControllerNamespace = "default"
	defaultNamespacePrefix     = "fsb-e2e"
	maxNamespaceLength         = 63
	defaultFastletImage        = "fast-sandbox/fastlet:dev"
)

var requireProfile = e2eenv.Require

type CleanupFunc func(context.Context) error

type Option func(*SuiteEnv)

type SuiteEnv struct {
	env                 envpkg.Environment
	cfg                 *envconf.Config
	controllerNamespace string
	namespacePrefix     string
	runID               string
	cleanups            []CleanupFunc
	namespaceCounter    uint64
}

func New(opts ...Option) *SuiteEnv {
	cfg := envconf.New()
	suiteEnv := &SuiteEnv{
		cfg:                 cfg,
		controllerNamespace: DefaultControllerNamespace,
		namespacePrefix:     defaultNamespacePrefix,
		runID:               newRunID(),
	}

	for _, opt := range opts {
		opt(suiteEnv)
	}

	if cfg.KubeconfigFile() == "" {
		if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
			cfg.WithKubeconfigFile(kubeconfig)
		}
	}
	if suiteEnv.controllerNamespace == "" {
		suiteEnv.controllerNamespace = DefaultControllerNamespace
	}
	if suiteEnv.namespacePrefix == "" {
		suiteEnv.namespacePrefix = defaultNamespacePrefix
	}

	suiteEnv.env = envpkg.NewWithConfig(cfg)
	return suiteEnv
}

func WithKubeconfig(path string) Option {
	return func(env *SuiteEnv) {
		env.cfg.WithKubeconfigFile(path)
	}
}

func WithNamespacePrefix(prefix string) Option {
	return func(env *SuiteEnv) {
		env.namespacePrefix = sanitizeDNSLabel(prefix)
	}
}

func WithControllerNamespace(namespace string) Option {
	return func(env *SuiteEnv) {
		env.controllerNamespace = strings.TrimSpace(namespace)
	}
}

func (e *SuiteEnv) Env() envpkg.Environment {
	return e.env
}

func (e *SuiteEnv) Config() *envconf.Config {
	return e.cfg
}

func (e *SuiteEnv) ControllerNamespace() string {
	return e.controllerNamespace
}

func (e *SuiteEnv) AllocateNamespace(name string) string {
	suffix := atomic.AddUint64(&e.namespaceCounter, 1)
	base := strings.Join(filterEmpty([]string{
		sanitizeDNSLabel(e.namespacePrefix),
		sanitizeDNSLabel(name),
	}), "-")
	uniqueSuffix := fmt.Sprintf("-%s-%d", e.runID, suffix)
	namespace := base + uniqueSuffix
	if len(namespace) <= maxNamespaceLength {
		return namespace
	}

	maxBaseLength := maxNamespaceLength - len(uniqueSuffix)
	if maxBaseLength < 1 {
		return namespace[len(namespace)-maxNamespaceLength:]
	}
	return strings.Trim(base[:maxBaseLength], "-") + uniqueSuffix
}

func (e *SuiteEnv) RegisterCleanup(fn CleanupFunc) {
	if fn == nil {
		return
	}
	e.cleanups = append(e.cleanups, fn)
}

func (e *SuiteEnv) RunCleanups(ctx context.Context) error {
	var firstErr error
	for i := len(e.cleanups) - 1; i >= 0; i-- {
		if err := e.cleanups[i](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	e.cleanups = nil
	return firstErr
}

func RequireBasic(t testing.TB) *e2eenv.Manager {
	t.Helper()
	return requireProfile(t, e2eenv.ProfileBasic)
}

func RequireGVisor(t testing.TB) *e2eenv.Manager {
	t.Helper()
	return requireProfile(t, e2eenv.ProfileGVisor)
}

func RequireKataQemu(t testing.TB) *e2eenv.Manager {
	t.Helper()
	return requireProfile(t, e2eenv.ProfileKataQemu)
}

func RequireKataClh(t testing.TB) *e2eenv.Manager {
	t.Helper()
	return requireProfile(t, e2eenv.ProfileKataClh)
}

func RequireKataFc(t testing.TB) *e2eenv.Manager {
	t.Helper()
	return requireProfile(t, e2eenv.ProfileKataFc)
}

func (e *SuiteEnv) MustKubeClient(t *testing.T) client.Client {
	t.Helper()

	cfg, err := conf.New(e.cfg.KubeconfigFile())
	if err != nil {
		t.Fatalf("resolve kubeconfig: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := apiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add fast-sandbox scheme: %v", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create kube client: %v", err)
	}
	return k8sClient
}

func DeleteNamespace(ctx context.Context, t *testing.T, kubeClient client.Client, namespace string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("FAST_SANDBOX_E2E_PRESERVE_NAMESPACE")) == "1" {
		t.Logf("preserving e2e namespace %s because FAST_SANDBOX_E2E_PRESERVE_NAMESPACE=1", namespace)
		return
	}

	ns := &corev1.Namespace{}
	ns.Name = namespace
	if err := kubeClient.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
		t.Fatalf("delete namespace %s: %v", namespace, err)
	}
}

func FastletImage() string {
	for _, key := range []string{"FAST_SANDBOX_FASTLET_IMAGE", "FASTLET_IMAGE"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return defaultFastletImage
}

// SmallSandboxResourceProfile keeps e2e Pool capacity arithmetic realistic
// while fitting several isolated Fastlet Pods on a single-node kind cluster.
func SmallSandboxResourceProfile() apiv1alpha1.SandboxResourceProfile {
	return apiv1alpha1.SandboxResourceProfile{
		CPU: resource.MustParse("50m"), Memory: resource.MustParse("64Mi"), PIDs: 64,
	}
}

func sanitizeDNSLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "suite"
	}

	var b strings.Builder
	lastDash := false
	for _, r := range value {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "suite"
	}
	return out
}

func filterEmpty(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func newRunID() string {
	var value [4]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%x", uint64(time.Now().UnixNano())&0xffffffff)
}
