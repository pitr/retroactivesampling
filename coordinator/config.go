package main

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"pitr.ca/retroactivesampling/coordinator/redis"
)

type Config struct {
	GRPCListen      string         `yaml:"grpc_listen"`
	RedisPrimary    redis.Config   `yaml:"redis_primary"`
	RedisReplicas   []redis.Config `yaml:"redis_replicas"`
	DecidedKeyTTL   time.Duration  `yaml:"decided_key_ttl"`
	MetricsListen   string         `yaml:"metrics_listen"`
	ShutdownTimeout time.Duration  `yaml:"shutdown_timeout"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
