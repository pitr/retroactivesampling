package evaluator

import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"

// PolicyType indicates the type of sampling policy.
type PolicyType string

const (
	AlwaysSample     PolicyType = "always_sample"
	Latency          PolicyType = "latency"
	NumericAttribute PolicyType = "numeric_attribute"
	Probabilistic    PolicyType = "probabilistic"
	StatusCode       PolicyType = "status_code"
	StringAttribute  PolicyType = "string_attribute"
	RateLimiting     PolicyType = "rate_limiting"
	And              PolicyType = "and"
	Not              PolicyType = "not"
	Drop             PolicyType = "drop"
	SpanCount        PolicyType = "span_count"
	TraceState       PolicyType = "trace_state"
	BooleanAttribute PolicyType = "boolean_attribute"
	OTTLCondition    PolicyType = "ottl_condition"
	BytesLimiting    PolicyType = "bytes_limiting"
	TraceFlags       PolicyType = "trace_flags"
	Composite        PolicyType = "composite"
)

// SharedPolicyCfg holds fields common to all policy types and sub-policy types.
type SharedPolicyCfg struct {
	Name                string              `mapstructure:"name"`
	Type                PolicyType          `mapstructure:"type"`
	LatencyCfg          LatencyCfg          `mapstructure:"latency"`
	NumericAttributeCfg NumericAttributeCfg `mapstructure:"numeric_attribute"`
	ProbabilisticCfg    ProbabilisticCfg    `mapstructure:"probabilistic"`
	StatusCodeCfg       StatusCodeCfg       `mapstructure:"status_code"`
	StringAttributeCfg  StringAttributeCfg  `mapstructure:"string_attribute"`
	RateLimitingCfg     RateLimitingCfg     `mapstructure:"rate_limiting"`
	BytesLimitingCfg    BytesLimitingCfg    `mapstructure:"bytes_limiting"`
	SpanCountCfg        SpanCountCfg        `mapstructure:"span_count"`
	TraceStateCfg       TraceStateCfg       `mapstructure:"trace_state"`
	BooleanAttributeCfg BooleanAttributeCfg `mapstructure:"boolean_attribute"`
	OTTLConditionCfg    OTTLConditionCfg    `mapstructure:"ottl_condition"`
}

// PolicyCfg holds the common configuration to all policies.
type PolicyCfg struct {
	SharedPolicyCfg `mapstructure:",squash"`
	AndCfg          AndCfg  `mapstructure:"and"`
	NotCfg          NotCfg  `mapstructure:"not"`
	DropCfg         DropCfg `mapstructure:"drop"`
}

// AndSubPolicyCfg holds configuration for policies under and/drop compound policies.
type AndSubPolicyCfg struct {
	SharedPolicyCfg `mapstructure:",squash"`
}

// NotSubPolicyCfg holds configuration for the policy under not compound policy.
type NotSubPolicyCfg struct {
	SharedPolicyCfg `mapstructure:",squash"`
}

type AndCfg struct {
	SubPolicyCfg []AndSubPolicyCfg `mapstructure:"and_sub_policy"`
}

type NotCfg struct {
	SubPolicy NotSubPolicyCfg `mapstructure:"not_sub_policy"`
}

type DropCfg struct {
	SubPolicyCfg []AndSubPolicyCfg `mapstructure:"drop_sub_policy"`
}

type LatencyCfg struct {
	ThresholdMs      int64 `mapstructure:"threshold_ms"`
	UpperThresholdMs int64 `mapstructure:"upper_threshold_ms"`
}

type NumericAttributeCfg struct {
	Key      string `mapstructure:"key"`
	MinValue int64  `mapstructure:"min_value"`
	MaxValue int64  `mapstructure:"max_value"`
}

type ProbabilisticCfg struct {
	HashSeed           uint32  `mapstructure:"hash_seed"`
	SamplingPercentage float64 `mapstructure:"sampling_percentage"`
}

type StatusCodeCfg struct {
	StatusCodes []string `mapstructure:"status_codes"`
}

type StringAttributeCfg struct {
	Key                  string   `mapstructure:"key"`
	Values               []string `mapstructure:"values"`
	EnabledRegexMatching bool     `mapstructure:"enabled_regex_matching"`
	CacheMaxSize         int      `mapstructure:"cache_max_size"`
}

type RateLimitingCfg struct {
	SpansPerSecond int64 `mapstructure:"spans_per_second"`
}

type BytesLimitingCfg struct {
	BytesPerSecond int64 `mapstructure:"bytes_per_second"`
	BurstCapacity  int64 `mapstructure:"burst_capacity"`
}

type SpanCountCfg struct {
	MinSpans int32 `mapstructure:"min_spans"`
	MaxSpans int32 `mapstructure:"max_spans"`
}

type BooleanAttributeCfg struct {
	Key   string `mapstructure:"key"`
	Value bool   `mapstructure:"value"`
}

type TraceStateCfg struct {
	Key    string   `mapstructure:"key"`
	Values []string `mapstructure:"values"`
}

type OTTLConditionCfg struct {
	ErrorMode           ottl.ErrorMode `mapstructure:"error_mode"`
	SpanConditions      []string       `mapstructure:"span"`
	SpanEventConditions []string       `mapstructure:"spanevent"`
}
