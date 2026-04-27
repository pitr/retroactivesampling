package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

type TLSConfig struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// Credentials returns gRPC server transport credentials, or (nil, nil) if c is
// nil (caller should use plain gRPC).
func (c *TLSConfig) Credentials() (credentials.TransportCredentials, error) {
	if c == nil {
		return nil, nil
	}
	if c.CertFile == "" {
		return nil, errors.New("server tls: cert_file is required")
	}
	if c.KeyFile == "" {
		return nil, errors.New("server tls: key_file is required")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server tls: load keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if c.ClientCAFile != "" {
		pem, err := os.ReadFile(c.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("server tls: read client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server tls: client_ca_file %q contains no PEM certs", c.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tlsCfg), nil
}
