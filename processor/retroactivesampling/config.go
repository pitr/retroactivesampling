package processor

import (
	"time"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferDBPath        string                 `mapstructure:"buffer_db_path"`
	BufferTTL           time.Duration          `mapstructure:"buffer_ttl"`
	DropTTL             time.Duration          `mapstructure:"drop_ttl"`
	InterestCacheTTL    time.Duration          `mapstructure:"interest_cache_ttl"`
	CoordinatorEndpoint string                 `mapstructure:"coordinator_endpoint"`
	Rules               []evaluator.RuleConfig `mapstructure:"rules"`
}
