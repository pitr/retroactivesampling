package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/server"
)

type Config struct {
	GRPCListen      string            `yaml:"grpc_listen"`
	LogLevel        slog.Level        `yaml:"log_level"`
	MetricsListen   string            `yaml:"metrics_listen"`
	ShutdownTimeout time.Duration     `yaml:"shutdown_timeout"`
	TLS             *server.TLSConfig `yaml:"tls"`
	Mode            ModeConfig        `yaml:"mode"`
}

type ModeConfig struct {
	Single *SingleConfig       `yaml:"single"`
	Redis  *RedisConfig        `yaml:"distributed"`
	Proxy  *proxy.ClientConfig `yaml:"proxy"`
}

func (m ModeConfig) active() (any, error) {
	var n int
	var result any
	if m.Single != nil {
		n++
		result = m.Single
	}
	if m.Redis != nil {
		n++
		result = m.Redis
	}
	if m.Proxy != nil {
		n++
		result = m.Proxy
	}
	if n != 1 {
		return nil, fmt.Errorf("exactly one mode must be configured (got %d)", n)
	}
	return result, nil
}

type SingleConfig struct {
	DecidedKeyTTL time.Duration `yaml:"decided_key_ttl"`
}

type RedisConfig struct {
	DecidedKeyTTL time.Duration  `yaml:"decided_key_ttl"`
	RedisPrimary  redis.Config   `yaml:"redis_primary"`
	RedisReplicas []redis.Config `yaml:"redis_replicas"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
