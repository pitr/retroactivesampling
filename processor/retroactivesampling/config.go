package processor

import (
	"time"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferDir           string                 `mapstructure:"buffer_dir"`
	BufferTTL           time.Duration          `mapstructure:"buffer_ttl"`
	DropTTL             time.Duration          `mapstructure:"drop_ttl"`
	InterestCacheTTL    time.Duration          `mapstructure:"interest_cache_ttl"`
	MaxBufferBytes      int64                  `mapstructure:"max_buffer_bytes"`
	CoordinatorEndpoint string                 `mapstructure:"coordinator_endpoint"`
	Rules               []evaluator.RuleConfig `mapstructure:"rules"`
}
