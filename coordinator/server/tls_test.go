package server_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/internal/testtls"
	"pitr.ca/retroactivesampling/coordinator/server"
)

func TestTLSConfig_Nil_ReturnsNilCreds(t *testing.T) {
	var cfg *server.TLSConfig
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	assert.Nil(t, creds)
}

func TestTLSConfig_ServerOnly(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	cfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key}
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	assert.NotNil(t, creds)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func TestTLSConfig_MTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	cfg := &server.TLSConfig{
		CertFile:     files.Cert,
		KeyFile:      files.Key,
		ClientCAFile: files.CACert,
	}
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	require.NotNil(t, creds)
}

func TestTLSConfig_MissingKeyFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{CertFile: files.Cert}
	_, err := cfg.Credentials()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_file")
}

func TestTLSConfig_MissingCertFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{KeyFile: files.Key}
	_, err := cfg.Credentials()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cert_file")
}

func TestTLSConfig_BadCAFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{
		CertFile:     files.Cert,
		KeyFile:      files.Key,
		ClientCAFile: "/nonexistent/ca.pem",
	}
	_, err := cfg.Credentials()
	require.Error(t, err)
}
