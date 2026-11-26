package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// testCACert generates a self-signed CA certificate with real DER and
// returns it with its PEM encoding.
func testCACert(t *testing.T, commonName string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testCACertificatePEM(t *testing.T) []byte {
	t.Helper()
	_, pemBytes := testCACert(t, "opensandbox-mitm-ca.test")
	return pemBytes
}

// expectedLineage computes the fingerprints the lineage must carry,
// independently of the code under test.
func expectedLineage(t *testing.T, certificate *x509.Certificate) mitmCALineage {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	certDigest := sha256.Sum256(certificate.Raw)
	spkiDigest := sha256.Sum256(spki)
	return mitmCALineage{
		CertSHA256: hex.EncodeToString(certDigest[:]),
		SPKISHA256: hex.EncodeToString(spkiDigest[:]),
		NotAfter:   certificate.NotAfter.UTC().Format(time.RFC3339),
	}
}

func stageMitmCertificate(t *testing.T, payload []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, apiv1alpha2.EgressMITMCertItemName), payload, 0o600); err != nil {
		t.Fatalf("stage mounted mitm certificate: %v", err)
	}
	t.Setenv(apiv1alpha2.EgressMITMCertDirEnv, dir)
	return dir
}

// TestParseCACertificates: the entry must consist solely of valid PEM
// CERTIFICATE blocks — private keys, other blocks, and trailing data never
// pass.
func TestParseCACertificates(t *testing.T) {
	valid := testCACertificatePEM(t)
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not really der")})
	tests := []struct {
		name      string
		payload   []byte
		wantError string
	}{
		{name: "accepts a PEM X.509 certificate", payload: valid},
		{
			name:      "rejects an empty entry",
			payload:   []byte("  \n"),
			wantError: "empty",
		},
		{
			name:      "rejects a private key",
			payload:   privateKey,
			wantError: "private key",
		},
		{
			name:      "rejects a certificate shipped with its key",
			payload:   append(append([]byte{}, valid...), privateKey...),
			wantError: "private key",
		},
		{
			name:      "rejects a non-certificate PEM block",
			payload:   pem.EncodeToMemory(&pem.Block{Type: "DATA", Bytes: []byte("junk")}),
			wantError: "non-CERTIFICATE PEM block",
		},
		{
			name:      "rejects trailing non-PEM data",
			payload:   append(append([]byte{}, valid...), []byte("trailing junk")...),
			wantError: "trailing non-PEM data",
		},
		{
			name:      "rejects a case-mangled private key banner",
			payload:   append(append([]byte{}, valid...), []byte("-----begin private key-----\nyy\n")...),
			wantError: "trailing non-PEM data",
		},
		{
			name:      "rejects a binary DER private key",
			payload:   append(append([]byte{}, valid...), []byte{0x30, 0x82, 0x04, 0xbf}...),
			wantError: "trailing non-PEM data",
		},
		{
			name:      "rejects non-PEM payload",
			payload:   []byte("hello"),
			wantError: "no PEM CERTIFICATE block",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCACertificates(test.payload)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("parseCACertificates: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("parseCACertificates error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

// TestParseCACertificatesOrder: every CERTIFICATE block is returned, in the
// order it appears in the entry.
func TestParseCACertificatesOrder(t *testing.T) {
	_, firstPEM := testCACert(t, "ca-one.test")
	_, secondPEM := testCACert(t, "ca-two.test")
	payload := append(append([]byte{}, firstPEM...), secondPEM...)
	certificates, err := parseCACertificates(payload)
	if err != nil {
		t.Fatalf("parseCACertificates: %v", err)
	}
	if len(certificates) != 2 {
		t.Fatalf("expected 2 certificates, got %d", len(certificates))
	}
	if certificates[0].Subject.CommonName != "ca-one.test" || certificates[1].Subject.CommonName != "ca-two.test" {
		t.Fatalf("certificates out of order: %q, %q",
			certificates[0].Subject.CommonName, certificates[1].Subject.CommonName)
	}
}

// TestInjectEgressMITMCertificate: the certificates land re-encoded at the
// contract path with their lineage; missing prerequisites and invalid
// entries fail the build.
func TestInjectEgressMITMCertificate(t *testing.T) {
	certificate, certPEM := testCACert(t, "opensandbox-mitm-ca.test")

	t.Run("bakes the certificate at the bootstrap contract path with lineage", func(t *testing.T) {
		stageMitmCertificate(t, certPEM)
		rootfs := t.TempDir()
		lineage, err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{Execd: "opensandbox/execd:1.1.0"}, rootfs)
		if err != nil {
			t.Fatalf("injectEgressMITMCertificate: %v", err)
		}
		payload, err := os.ReadFile(filepath.Join(rootfs, strings.TrimPrefix(guestMITMCertPath, "/")))
		if err != nil {
			t.Fatalf("read baked certificate: %v", err)
		}
		if string(payload) != string(certPEM) {
			t.Fatalf("baked certificate differs from the canonical encoding")
		}
		want := expectedLineage(t, certificate)
		if len(lineage) != 1 || lineage[0] != want {
			t.Fatalf("lineage = %+v, want [%+v]", lineage, want)
		}
	})

	t.Run("bakes only the parsed certificates, ignoring nothing", func(t *testing.T) {
		_, firstPEM := testCACert(t, "ca-one.test")
		_, secondPEM := testCACert(t, "ca-two.test")
		stageMitmCertificate(t, append(append([]byte{}, firstPEM...), secondPEM...))
		rootfs := t.TempDir()
		lineage, err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{Execd: "e"}, rootfs)
		if err != nil {
			t.Fatalf("injectEgressMITMCertificate: %v", err)
		}
		payload, err := os.ReadFile(filepath.Join(rootfs, strings.TrimPrefix(guestMITMCertPath, "/")))
		if err != nil {
			t.Fatalf("read baked certificates: %v", err)
		}
		if string(payload) != string(append(append([]byte{}, firstPEM...), secondPEM...)) {
			t.Fatalf("baked payload must be the canonical re-encoding of the parsed certificates")
		}
		if len(lineage) != 2 {
			t.Fatalf("expected two lineage entries, got %+v", lineage)
		}
	})

	t.Run("fails when spec.execd is empty", func(t *testing.T) {
		stageMitmCertificate(t, certPEM)
		_, err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "requires spec.execd") {
			t.Fatalf("injectEgressMITMCertificate error = %v, want execd requirement error", err)
		}
	})

	t.Run("fails without the controller mount env", func(t *testing.T) {
		t.Setenv(apiv1alpha2.EgressMITMCertDirEnv, "")
		_, err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{Execd: "e"}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), apiv1alpha2.EgressMITMCertDirEnv+" is unset") {
			t.Fatalf("injectEgressMITMCertificate error = %v, want missing-mount error", err)
		}
	})

	t.Run("fails on a private key entry", func(t *testing.T) {
		stageMitmCertificate(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))
		_, err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{Execd: "e"}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "private key") {
			t.Fatalf("injectEgressMITMCertificate error = %v, want private-key rejection", err)
		}
	})
}

// TestInjectRuntimeBakesMitmCertificateAndGate: the gate line follows the
// merged envs, so it wins over spec.envs, and the lineage is returned.
func TestInjectRuntimeBakesMitmCertificateAndGate(t *testing.T) {
	stageMitmCertificate(t, testCACertificatePEM(t))
	workdir, rootfs := t.TempDir(), t.TempDir()
	// injectRuntime assumes a real-rootfs shape: etc must exist.
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("stage etc: %v", err)
	}
	spec := apiv1alpha2.SandboxTemplateSpec{
		Execd: "opensandbox/execd:1.1.0",
		Envs: []corev1.EnvVar{
			{Name: guestMITMTrustGate, Value: "false"},
			{Name: guestMITMStabilizeWindow, Value: "15"},
			{Name: guestMITMWatchInterval, Value: "30"},
		},
		EgressMITMCertificate: &apiv1alpha2.MITMCertificateSpec{
			SecretRef: corev1.LocalObjectReference{Name: "mitm-ca"},
			Key:       "ca.crt",
		},
	}
	mitmCA, err := injectRuntime(spec, workdir, rootfs)
	if err != nil {
		t.Fatalf("injectRuntime: %v", err)
	}
	if len(mitmCA) != 1 {
		t.Fatalf("expected one lineage entry, got %+v", mitmCA)
	}
	payload, err := os.ReadFile(filepath.Join(rootfs, strings.TrimPrefix(guestMITMCertPath, "/")))
	if err != nil {
		t.Fatalf("baked certificate missing: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("baked certificate is empty")
	}
	envPayload, err := os.ReadFile(filepath.Join(rootfs, "etc", "sandbox-init.env"))
	if err != nil {
		t.Fatalf("read sandbox-init.env: %v", err)
	}
	envContent := string(envPayload)
	for _, check := range []struct {
		name     string
		userLine string
		baked    string
	}{
		{"trust gate", "export " + guestMITMTrustGate + "='false'", "export " + guestMITMTrustGate + "=true"},
		{"stabilize window", "export " + guestMITMStabilizeWindow + "='15'", "export " + guestMITMStabilizeWindow + "=0"},
		{"watch interval", "export " + guestMITMWatchInterval + "='30'", "export " + guestMITMWatchInterval + "=0"},
	} {
		userIdx := strings.Index(envContent, check.userLine)
		bakedIdx := strings.LastIndex(envContent, check.baked)
		if userIdx == -1 || bakedIdx == -1 || bakedIdx < userIdx {
			t.Fatalf("sandbox-init.env must carry %s after user env (user at %d, baked at %d):\n%s",
				check.name, userIdx, bakedIdx, envContent)
		}
	}
}

// TestInjectRuntimeWithoutMitmCertificate: no CA configured, no lineage or MITM envs.
func TestInjectRuntimeWithoutMitmCertificate(t *testing.T) {
	workdir, rootfs := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("stage etc: %v", err)
	}
	mitmCA, err := injectRuntime(apiv1alpha2.SandboxTemplateSpec{}, workdir, rootfs)
	if err != nil {
		t.Fatalf("injectRuntime: %v", err)
	}
	if mitmCA != nil {
		t.Fatalf("expected nil lineage without egressMitmCertificate, got %+v", mitmCA)
	}
	envPayload, err := os.ReadFile(filepath.Join(rootfs, "etc", "sandbox-init.env"))
	if err != nil {
		t.Fatalf("read sandbox-init.env: %v", err)
	}
	envContent := string(envPayload)
	if strings.Contains(envContent, guestMITMTrustGate) || strings.Contains(envContent, guestMITMStabilizeWindow) || strings.Contains(envContent, guestMITMWatchInterval) {
		t.Fatalf("sandbox-init.env must not set MITM vars without egressMitmCertificate:\n%s", envContent)
	}
}

func TestRenderGuestInitMITMCAReadinessGate(t *testing.T) {
	withoutCA := renderGuestInit(apiv1alpha2.SandboxTemplateSpec{Execd: "opensandbox/execd:1.1.0"})
	if strings.Contains(withoutCA, "mitm_ca_not_installed") {
		t.Fatalf("init must omit MITM CA readiness gate when egressMitmCertificate is unset")
	}

	withCA := renderGuestInit(apiv1alpha2.SandboxTemplateSpec{
		Execd: "opensandbox/execd:1.1.0",
		EgressMITMCertificate: &apiv1alpha2.MITMCertificateSpec{
			SecretRef: corev1.LocalObjectReference{Name: "mitm-ca"},
			Key:       "ca.crt",
		},
	})
	gateIdx := strings.Index(withCA, `echo "SANDBOX_STARTUP_FAILED mitm_ca_not_installed"`)
	readyIdx := strings.Index(withCA, "echo SANDBOX_READY")
	if gateIdx == -1 || readyIdx == -1 || gateIdx > readyIdx {
		t.Fatalf("MITM CA readiness gate must appear before SANDBOX_READY (gate=%d, ready=%d)", gateIdx, readyIdx)
	}
}

// Execute the rendered MITM CA readiness gate with real sh under set -eu to
// verify Debian, RHEL, and minimal (no-tooling) trust-store outcomes.
func TestMITMCAReadinessGateBehavior(t *testing.T) {
	certPEM := testCACertificatePEM(t)
	otherPEM := testCACertificatePEM(t)
	gate := renderMITMCAReadinessGate(apiv1alpha2.SandboxTemplateSpec{
		EgressMITMCertificate: &apiv1alpha2.MITMCertificateSpec{
			SecretRef: corev1.LocalObjectReference{Name: "mitm-ca"},
		},
	})

	tests := []struct {
		name        string
		tool        string
		anchor      []byte
		bundle      []byte
		wantLog     string
		wantFailure bool
	}{
		{name: "debian installed", tool: "update-ca-certificates", anchor: certPEM, bundle: append([]byte("root\n"), certPEM...), wantLog: "mitm CA verified in system trust store (update-ca-certificates)"},
		{name: "debian missing anchor", tool: "update-ca-certificates", bundle: certPEM, wantFailure: true},
		{name: "debian missing from bundle", tool: "update-ca-certificates", anchor: certPEM, bundle: otherPEM, wantFailure: true},
		{name: "rhel installed", tool: "update-ca-trust", anchor: certPEM, bundle: append([]byte("root\n"), certPEM...), wantLog: "mitm CA verified in system trust store (update-ca-trust)"},
		{name: "rhel missing from bundle", tool: "update-ca-trust", anchor: certPEM, bundle: otherPEM, wantFailure: true},
		{name: "minimal image without trust tooling", tool: "", wantLog: "mitm CA trust-store check skipped (no update-ca-certificates or update-ca-trust)"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}
			for _, name := range []string{"grep", "head", "tail"} {
				src, err := exec.LookPath(name)
				if err != nil {
					t.Fatalf("lookpath %s: %v", name, err)
				}
				if err := os.Symlink(src, filepath.Join(binDir, name)); err != nil {
					t.Fatalf("symlink %s: %v", name, err)
				}
			}
			if test.tool != "" {
				if err := os.WriteFile(filepath.Join(binDir, test.tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatalf("write stub %s: %v", test.tool, err)
				}
			}

			bakedPath := filepath.Join(root, "mitmproxy-ca-cert.pem")
			if err := os.WriteFile(bakedPath, certPEM, 0o644); err != nil {
				t.Fatalf("write baked cert: %v", err)
			}
			debAnchor := filepath.Join(root, "opensandbox-mitmproxy-ca.crt")
			debBundle := filepath.Join(root, "ca-certificates.crt")
			rhelAnchor := filepath.Join(root, "opensandbox-mitmproxy-ca.pem")
			rhelBundle := filepath.Join(root, "tls-ca-bundle.pem")
			if test.tool == "update-ca-certificates" {
				if len(test.anchor) > 0 {
					if err := os.WriteFile(debAnchor, test.anchor, 0o644); err != nil {
						t.Fatalf("write debian anchor: %v", err)
					}
				}
				if len(test.bundle) > 0 {
					if err := os.WriteFile(debBundle, test.bundle, 0o644); err != nil {
						t.Fatalf("write debian bundle: %v", err)
					}
				}
			} else if test.tool == "update-ca-trust" {
				if len(test.anchor) > 0 {
					if err := os.WriteFile(rhelAnchor, test.anchor, 0o644); err != nil {
						t.Fatalf("write rhel anchor: %v", err)
					}
				}
				if len(test.bundle) > 0 {
					if err := os.WriteFile(rhelBundle, test.bundle, 0o644); err != nil {
						t.Fatalf("write rhel bundle: %v", err)
					}
				}
			}

			script := "set -eu\n" + gate + "echo GATE_OK\n"
			script = strings.ReplaceAll(script, guestMITMCertPath, bakedPath)
			script = strings.ReplaceAll(script, "/usr/local/share/ca-certificates/opensandbox-mitmproxy-ca.crt", debAnchor)
			script = strings.ReplaceAll(script, "/etc/ssl/certs/ca-certificates.crt", debBundle)
			script = strings.ReplaceAll(script, "/etc/pki/ca-trust/source/anchors/opensandbox-mitmproxy-ca.pem", rhelAnchor)
			script = strings.ReplaceAll(script, "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", rhelBundle)

			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = []string{"PATH=" + binDir}
			out, err := cmd.CombinedOutput()
			if test.wantFailure {
				if err == nil {
					t.Fatalf("expected gate failure, got success:\n%s", out)
				}
				if !strings.Contains(string(out), "SANDBOX_STARTUP_FAILED mitm_ca_not_installed") {
					t.Fatalf("expected mitm_ca_not_installed marker, got:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected gate success, got %v:\n%s", err, out)
			}
			if !strings.Contains(string(out), "GATE_OK") {
				t.Fatalf("expected GATE_OK, got:\n%s", out)
			}
			if test.wantLog != "" && !strings.Contains(string(out), test.wantLog) {
				t.Fatalf("expected log %q, got:\n%s", test.wantLog, out)
			}
		})
	}
}
