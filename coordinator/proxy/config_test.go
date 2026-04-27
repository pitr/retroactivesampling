package proxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/proxy"
)

func TestClientConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     proxy.ClientConfig
		wantErr string
	}{
		{
			name: "minimal",
			cfg:  proxy.ClientConfig{Endpoint: "host:9090"},
		},
		{
			name:    "missing endpoint",
			cfg:     proxy.ClientConfig{},
			wantErr: "endpoint",
		},
		{
			name: "tls server-only",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{CAFile: "/etc/ssl/ca.pem"},
			},
		},
		{
			name: "tls cert without key",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{CertFile: "/etc/ssl/c.pem"},
			},
			wantErr: "key_file",
		},
		{
			name: "tls key without cert",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{KeyFile: "/etc/ssl/k.pem"},
			},
			wantErr: "cert_file",
		},
		{
			name: "compression unknown",
			cfg: proxy.ClientConfig{
				Endpoint:    "host:9090",
				Compression: "snappy",
			},
			wantErr: "compression",
		},
		{
			name: "compression gzip",
			cfg: proxy.ClientConfig{
				Endpoint:    "host:9090",
				Compression: "gzip",
			},
		},
		{
			name: "header invalid uppercase",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"Authorization": "Bearer x"},
			},
			wantErr: "header",
		},
		{
			name: "header reserved grpc- prefix",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"grpc-trace-bin": "x"},
			},
			wantErr: "grpc-",
		},
		{
			name: "header valid",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"authorization": "Bearer x", "x-tenant-id": "t"},
			},
		},
		{
			name: "keepalive timeout >= time",
			cfg: proxy.ClientConfig{
				Endpoint:  "host:9090",
				KeepAlive: &proxy.KeepAliveConfig{Time: 10 * time.Second, Timeout: 10 * time.Second},
			},
			wantErr: "timeout",
		},
		{
			name: "keepalive valid",
			cfg: proxy.ClientConfig{
				Endpoint:  "host:9090",
				KeepAlive: &proxy.KeepAliveConfig{Time: 30 * time.Second, Timeout: 10 * time.Second},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
