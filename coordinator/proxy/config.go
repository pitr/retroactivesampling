package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // register gzip codec
	"google.golang.org/grpc/keepalive"
)

type ClientConfig struct {
	Endpoint    string            `yaml:"endpoint"`
	TLS         *TLSConfig        `yaml:"tls"`
	Headers     map[string]string `yaml:"headers"`
	Compression string            `yaml:"compression"`
	KeepAlive   *KeepAliveConfig  `yaml:"keepalive"`
}

type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	ServerNameOverride string `yaml:"server_name_override"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

type KeepAliveConfig struct {
	Time                time.Duration `yaml:"time"`
	Timeout             time.Duration `yaml:"timeout"`
	PermitWithoutStream bool          `yaml:"permit_without_stream"`
}

func (c *ClientConfig) Validate() error {
	if c.Endpoint == "" {
		return errors.New("proxy: endpoint is required")
	}
	if c.TLS != nil {
		if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
			if c.TLS.CertFile == "" {
				return errors.New("proxy tls: cert_file is required when key_file is set")
			}
			return errors.New("proxy tls: key_file is required when cert_file is set")
		}
	}
	if c.Compression != "" && c.Compression != "gzip" {
		return fmt.Errorf("proxy: unsupported compression %q (only \"gzip\" or empty)", c.Compression)
	}
	for k := range c.Headers {
		if err := validateHeaderKey(k); err != nil {
			return err
		}
	}
	if c.KeepAlive != nil && c.KeepAlive.Time > 0 && c.KeepAlive.Timeout > 0 && c.KeepAlive.Timeout >= c.KeepAlive.Time {
		return errors.New("proxy keepalive: timeout must be less than time")
	}
	return nil
}

func validateHeaderKey(k string) error {
	if k == "" {
		return errors.New("proxy: header name must not be empty")
	}
	if strings.HasPrefix(k, "grpc-") {
		return fmt.Errorf("proxy: header %q uses reserved grpc- prefix", k)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return fmt.Errorf("proxy: header %q has invalid character; must be lowercase a-z, 0-9, '-', '_', '.'", k)
		}
	}
	return nil
}

func (c *ClientConfig) dialOptions() ([]grpc.DialOption, error) {
	var opts []grpc.DialOption
	creds, err := c.transportCredentials()
	if err != nil {
		return nil, err
	}
	opts = append(opts, grpc.WithTransportCredentials(creds))
	if c.Compression == "gzip" {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")))
	}
	if kp := c.KeepAlive; kp != nil {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                kp.Time,
			Timeout:             kp.Timeout,
			PermitWithoutStream: kp.PermitWithoutStream,
		}))
	}
	return opts, nil
}

func (c *ClientConfig) transportCredentials() (credentials.TransportCredentials, error) {
	if c.TLS == nil {
		return insecure.NewCredentials(), nil
	}
	if c.TLS.InsecureSkipVerify {
		slog.Warn("proxy tls: insecure_skip_verify enabled; server certificate will not be validated", "endpoint", c.Endpoint)
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.TLS.InsecureSkipVerify, // #nosec G402
		ServerName:         c.TLS.ServerNameOverride,
	}
	if c.TLS.CAFile != "" {
		pem, err := os.ReadFile(c.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("proxy tls: read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("proxy tls: ca_file %q contains no PEM certs", c.TLS.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if c.TLS.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("proxy tls: load keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(tlsCfg), nil
}
