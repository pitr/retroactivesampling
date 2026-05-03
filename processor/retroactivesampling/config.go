package retroactivesampling

import (
	"time"

	"go.opentelemetry.io/collector/config/configgrpc"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferFile       string                  `mapstructure:"buffer_file"`
	MaxBufferBytes   int64                   `mapstructure:"max_buffer_bytes"`
	DecisionWaitTime time.Duration           `mapstructure:"decision_wait_time"`
	CoordinatorGRPC  configgrpc.ClientConfig `mapstructure:"coordinator_grpc"`
	Policies         []evaluator.PolicyCfg   `mapstructure:"policies"`
}

func (c *Config) Validate() error {
	return c.CoordinatorGRPC.Validate()
}
