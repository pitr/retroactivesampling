package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"pitr.ca/retroactivesampling/coordinator/redis"
)

type Config struct {
	GRPCListen      string        `yaml:"grpc_listen"`
	DecidedKeyTTL   time.Duration `yaml:"decided_key_ttl"`
	MetricsListen   string        `yaml:"metrics_listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Mode            ModeConfig    `yaml:"mode"`
}

type ModeConfig struct {
	Single      *SingleConfig      `yaml:"single"`
	Distributed *DistributedConfig `yaml:"distributed"`
}

func (m ModeConfig) active() (any, error) {
	var results []any
	if m.Single != nil {
		results = append(results, m.Single)
	}
	if m.Distributed != nil {
		results = append(results, m.Distributed)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("exactly one mode must be configured (got %d)", len(results))
	}
	return results[0], nil
}

type SingleConfig struct{}

type DistributedConfig struct {
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
