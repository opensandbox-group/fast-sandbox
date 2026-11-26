package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
)

// guestMITMCertPath is the execd bootstrap contract path the certificate
// must land at for the runtime trust flow to fire.
const guestMITMCertPath = "/opt/opensandbox/mitmproxy-ca-cert.pem"

// guestMITMTrustGate enables the bootstrap's MITM trust installation
// (system store, merged bundle, NSS, JDK keystores, client env vars).
const guestMITMTrustGate = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"

// guestMITMStabilizeWindow overrides execd bootstrap's default 15s CA
// stabilization wait. Firecracker VMs bake the CA statically into the rootfs
// rather than sharing a dynamic sidecar volume, so waiting for an egress
// purge/re-export would stall every template build and RootfsOnly cold boot.
const guestMITMStabilizeWindow = "MITM_CA_STABILIZE_WINDOW"

// guestMITMWatchInterval disables execd bootstrap's background 30s CA rotation
// watcher. Baked VM CAs never rotate in place, so leaving the watcher armed
// would freeze an idle sleep loop into every template snapshot.
const guestMITMWatchInterval = "MITM_CA_WATCH_INTERVAL"

// renderMITMCAReadinessGate emits a shell gate that fails the template build
// before SANDBOX_READY if a baked MITM CA did not land in the guest's system
// trust store (e.g. update-ca-certificates failed or readiness fired before
// bootstrap finished trusting the CA).
func renderMITMCAReadinessGate(spec apiv1alpha2.SandboxTemplateSpec) string {
	if spec.EgressMITMCertificate == nil {
		return ""
	}
	// Use the penultimate base64 line (inside the certificate signature) rather
	// than line 1, which can collide across X.509 certs sharing ASN.1 headers.
	return `mitm_ca_line="$(grep -v '^-----' ` + guestMITMCertPath + ` 2>/dev/null | tail -n 2 | head -n 1 || true)"
if [ -z "$mitm_ca_line" ]; then
  echo "SANDBOX_STARTUP_FAILED mitm_ca_not_installed"
  exit 1
fi
if command -v update-ca-certificates >/dev/null 2>&1; then
  if [ ! -s /usr/local/share/ca-certificates/opensandbox-mitmproxy-ca.crt ] || ! grep -Fq "$mitm_ca_line" /etc/ssl/certs/ca-certificates.crt 2>/dev/null; then
    echo "SANDBOX_STARTUP_FAILED mitm_ca_not_installed"
    exit 1
  fi
  echo "mitm CA verified in system trust store (update-ca-certificates)"
elif command -v update-ca-trust >/dev/null 2>&1; then
  if [ ! -s /etc/pki/ca-trust/source/anchors/opensandbox-mitmproxy-ca.pem ] || ! grep -Fq "$mitm_ca_line" /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem 2>/dev/null; then
    echo "SANDBOX_STARTUP_FAILED mitm_ca_not_installed"
    exit 1
  fi
  echo "mitm CA verified in system trust store (update-ca-trust)"
else
  echo "mitm CA trust-store check skipped (no update-ca-certificates or update-ca-trust)"
fi
`
}

// mitmCALineage is one baked certificate's public-key identity for the
// manifest lineage (lineage.egressMitmCA.certificates[]) — fingerprints
// only, never key or Secret material.
type mitmCALineage = artifacts.CertificateFingerprint

// injectEgressMITMCertificate bakes the mounted MITM CA certificates into
// the guest rootfs at the bootstrap contract path and returns their lineage
// fingerprints. The trust installation itself stays owned by the execd
// bootstrap script, which runs at this build's snapshot boot.
func injectEgressMITMCertificate(spec apiv1alpha2.SandboxTemplateSpec, mountPoint string) ([]mitmCALineage, error) {
	if spec.Execd == "" {
		return nil, errors.New("spec.egressMitmCertificate requires spec.execd: the guest trust installation lives in the execd bootstrap")
	}
	dir := os.Getenv(apiv1alpha2.EgressMITMCertDirEnv)
	if dir == "" {
		return nil, errors.New("egressMitmCertificate is set but the build Pod did not mount it: " + apiv1alpha2.EgressMITMCertDirEnv + " is unset")
	}
	payload, err := os.ReadFile(filepath.Join(dir, apiv1alpha2.EgressMITMCertItemName))
	if err != nil {
		return nil, fmt.Errorf("read mounted mitm certificate: %w", err)
	}
	certificates, err := parseCACertificates(payload)
	if err != nil {
		return nil, err
	}
	// Bake the re-encoded certificates, never the entry verbatim: the
	// published bytes are then exactly what the lineage fingerprints cover.
	baked := encodeCertificates(certificates)
	target := filepath.Join(mountPoint, strings.TrimPrefix(guestMITMCertPath, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(target, baked, 0o644); err != nil { //nolint:gosec // guest CA certificate, world-readable by design
		return nil, err
	}
	return certificateLineage(certificates)
}

// parseCACertificates is the second gate after the controller's key-only
// projection: the entry must consist solely of PEM CERTIFICATE blocks (plus
// whitespace) and hold at least one valid X.509 certificate — private-key
// blocks, other PEM blocks, and trailing non-PEM data are rejected outright.
// CA constraints (BasicConstraints CA:TRUE) are not verified.
func parseCACertificates(payload []byte) ([]*x509.Certificate, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, errors.New("mounted mitm certificate entry is empty")
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) {
		return nil, errors.New("mounted mitm certificate entry contains a private key; only certificates may be baked")
	}
	var certificates []*x509.Certificate
	rest := payload
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("mounted mitm certificate entry holds a non-CERTIFICATE PEM block %q", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mitm certificate entry is not a valid X.509 certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, errors.New("mitm certificate entry holds no PEM CERTIFICATE block")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("mitm certificate entry holds trailing non-PEM data; only PEM CERTIFICATE blocks may be baked")
	}
	return certificates, nil
}

// encodeCertificates re-encodes the parsed certificates as canonical PEM.
func encodeCertificates(certificates []*x509.Certificate) []byte {
	var baked []byte
	for _, certificate := range certificates {
		baked = append(baked, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
	}
	return baked
}

// certificateLineage fingerprints the parsed certificates for the manifest
// lineage.
func certificateLineage(certificates []*x509.Certificate) ([]mitmCALineage, error) {
	lineage := make([]mitmCALineage, 0, len(certificates))
	for _, certificate := range certificates {
		spki, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("fingerprint mitm certificate %q: %w", certificate.Subject, err)
		}
		certDigest := sha256.Sum256(certificate.Raw)
		spkiDigest := sha256.Sum256(spki)
		lineage = append(lineage, mitmCALineage{
			CertSHA256: hex.EncodeToString(certDigest[:]),
			SPKISHA256: hex.EncodeToString(spkiDigest[:]),
			NotAfter:   certificate.NotAfter.UTC().Format(time.RFC3339),
		})
	}
	return lineage, nil
}
