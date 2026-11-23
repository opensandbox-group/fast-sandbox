package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

func fastPathTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("FastPath TLS certificate and key are required")
	}
	var (
		mu                        sync.Mutex
		cached                    *tls.Certificate
		certModTime, keyModTime   time.Time
		certFileSize, keyFileSize int64
	)
	load := func() (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		certInfo, err := os.Stat(certFile)
		if err != nil {
			return nil, err
		}
		keyInfo, err := os.Stat(keyFile)
		if err != nil {
			return nil, err
		}
		if cached != nil && certInfo.ModTime().Equal(certModTime) && certInfo.Size() == certFileSize && keyInfo.ModTime().Equal(keyModTime) && keyInfo.Size() == keyFileSize {
			return cached, nil
		}
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cached = &pair
		certModTime, certFileSize = certInfo.ModTime(), certInfo.Size()
		keyModTime, keyFileSize = keyInfo.ModTime(), keyInfo.Size()
		return cached, nil
	}
	if _, err := load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return load()
		},
	}, nil
}
