package redis

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
}

type Config struct {
	Transport    string        `yaml:"transport"`    // tcp (default) or unix
	Endpoint     string        `yaml:"endpoint"`
	ClientName   string        `yaml:"client_name"`
	Username     string        `yaml:"username"`
	Password     string        `yaml:"password"`
	DB           int           `yaml:"db"`
	MaxRetries   int           `yaml:"max_retries"`
	DialTimeout  time.Duration `yaml:"dial_timeout"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	TLS          TLSConfig     `yaml:"tls"`
}

func (c Config) options() (*redis.Options, error) {
	opts := &redis.Options{
		Network:      c.Transport,
		Addr:         c.Endpoint,
		ClientName:   c.ClientName,
		Username:     c.Username,
		Password:     c.Password,
		DB:           c.DB,
		MaxRetries:   c.MaxRetries,
		DialTimeout:  c.DialTimeout,
		ReadTimeout:  c.ReadTimeout,
		WriteTimeout: c.WriteTimeout,
	}
	if c.TLS.Enabled {
		tlsCfg := &tls.Config{InsecureSkipVerify: c.TLS.InsecureSkipVerify} // #nosec G402
		if c.TLS.CAFile != "" {
			pem, err := os.ReadFile(c.TLS.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg.RootCAs = pool
		}
		if c.TLS.CertFile != "" || c.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
			if err != nil {
				return nil, err
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		opts.TLSConfig = tlsCfg
	}
	return opts, nil
}
