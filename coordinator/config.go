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
	var n int
	var result any
	if m.Single != nil {
		n++
		result = m.Single
	}
	if m.Distributed != nil {
		n++
		result = m.Distributed
	}
	if n != 1 {
		return nil, fmt.Errorf("exactly one mode must be configured (got %d)", n)
	}
	return result, nil
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
