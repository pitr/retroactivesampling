package processor

import (
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferFile              string                 `mapstructure:"buffer_file"`
	MaxBufferBytes          int64                  `mapstructure:"max_buffer_bytes"`
	MaxInterestCacheEntries int                    `mapstructure:"max_interest_cache_entries"`
	CoordinatorEndpoint     string                 `mapstructure:"coordinator_endpoint"`
	Rules                   []evaluator.RuleConfig `mapstructure:"rules"`
}
