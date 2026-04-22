package retroactivesampling

import (
	"go.opentelemetry.io/collector/config/configgrpc"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferFile              string                  `mapstructure:"buffer_file"`
	MaxBufferBytes          int64                   `mapstructure:"max_buffer_bytes"`
	MaxInterestCacheEntries int                     `mapstructure:"max_interest_cache_entries"`
	CoordinatorGRPC         configgrpc.ClientConfig `mapstructure:"coordinator_grpc"`
	Policies                []evaluator.PolicyCfg   `mapstructure:"policies"`
}

func (c *Config) Validate() error {
	return c.CoordinatorGRPC.Validate()
}
