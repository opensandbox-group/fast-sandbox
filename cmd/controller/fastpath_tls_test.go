package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFastPathTLSReloadsServingCertificate(t *testing.T) {
	certFile, keyFile := filepath.Join(t.TempDir(), "tls.crt"), filepath.Join(t.TempDir(), "tls.key")
	write := func(serial int64) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "fastpath.test"}, DNSNames: []string{"fastpath.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		require.NoError(t, err)
		encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}), 0600))
		require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	}
	write(1)
	config, err := fastPathTLS(certFile, keyFile)
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), config.MinVersion)
	certificate, err := config.GetCertificate(&tls.ClientHelloInfo{})
	require.NoError(t, err)
	require.EqualValues(t, 1, certificate.Leaf.SerialNumber.Int64())
	write(2)
	certificate, err = config.GetCertificate(&tls.ClientHelloInfo{})
	require.NoError(t, err)
	require.EqualValues(t, 2, certificate.Leaf.SerialNumber.Int64())
	require.NoError(t, os.WriteFile(certFile, []byte("invalid certificate"), 0600))
	_, err = config.GetCertificate(&tls.ClientHelloInfo{})
	require.Error(t, err)
	_, err = fastPathTLS("", "")
	require.Error(t, err)
}
