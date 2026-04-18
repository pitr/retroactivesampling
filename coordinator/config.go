package main

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GRPCListen    string        `yaml:"grpc_listen"`
	RedisAddr     string        `yaml:"redis_addr"`
	DecidedKeyTTL time.Duration `yaml:"decided_key_ttl"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
