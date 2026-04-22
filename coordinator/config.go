package main

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GRPCListen          string        `yaml:"grpc_listen"`
	RedisAddr           string        `yaml:"redis_addr"`
	RedisReplicaAddrs   []string      `yaml:"redis_replica_addrs"`
	DecidedKeyTTL       time.Duration `yaml:"decided_key_ttl"`
	MetricsListen       string        `yaml:"metrics_listen"`
	ShutdownTimeout     time.Duration `yaml:"shutdown_timeout"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
