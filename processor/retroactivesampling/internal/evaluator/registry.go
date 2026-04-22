package evaluator

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// Build constructs a Chain from a slice of PolicyCfg.
// Returns an error for unsupported types (span_count, rate_limiting, bytes_limiting, composite)
// or unknown types.
func Build(settings component.TelemetrySettings, policies []PolicyCfg) (Chain, error) {
	chain := make(Chain, 0, len(policies))
	for i := range policies {
		ev, err := buildPolicy(settings, &policies[i])
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", policies[i].Name, err)
		}
		chain = append(chain, ev)
	}
	return chain, nil
}

func buildPolicy(settings component.TelemetrySettings, cfg *PolicyCfg) (Evaluator, error) {
	switch cfg.Type {
	case And:
		return buildAndPolicy(settings, &cfg.AndCfg)
	case Not:
		return buildNotPolicy(settings, &cfg.NotCfg)
	case Drop:
		return buildDropPolicy(settings, &cfg.DropCfg)
	default:
		return buildShared(settings, &cfg.SharedPolicyCfg)
	}
}

func buildShared(settings component.TelemetrySettings, cfg *SharedPolicyCfg) (Evaluator, error) {
	logger := settings.Logger
	switch cfg.Type {
	case AlwaysSample:
		return NewAlwaysSample(logger), nil
	case Latency:
		return NewLatency(logger, cfg.LatencyCfg.ThresholdMs, cfg.LatencyCfg.UpperThresholdMs), nil
	case StatusCode:
		return NewStatusCodeFilter(logger, cfg.StatusCodeCfg.StatusCodes)
	case StringAttribute:
		c := cfg.StringAttributeCfg
		return NewStringAttributeFilter(logger, c.Key, c.Values, c.EnabledRegexMatching, c.CacheMaxSize)
	case NumericAttribute:
		c := cfg.NumericAttributeCfg
		var minPtr, maxPtr *int64
		if c.MinValue != 0 {
			minPtr = &c.MinValue
		}
		if c.MaxValue != 0 {
			maxPtr = &c.MaxValue
		}
		return NewNumericAttributeFilter(logger, c.Key, minPtr, maxPtr), nil
	case BooleanAttribute:
		c := cfg.BooleanAttributeCfg
		return NewBooleanAttributeFilter(logger, c.Key, c.Value), nil
	case Probabilistic:
		c := cfg.ProbabilisticCfg
		return NewProbabilisticSampler(logger, c.HashSalt, c.SamplingPercentage), nil
	case TraceState:
		return NewTraceStateFilter(logger, cfg.TraceStateCfg.Key, cfg.TraceStateCfg.Values), nil
	case TraceFlags:
		return NewTraceFlags(logger), nil
	case OTTLCondition:
		c := cfg.OTTLConditionCfg
		return NewOTTLConditionFilter(settings, c.SpanConditions, c.SpanEventConditions, c.ErrorMode)
	case SpanCount, RateLimiting, BytesLimiting, Composite:
		return nil, fmt.Errorf("policy type %q requires complete trace data and is not supported", cfg.Type)
	default:
		return nil, fmt.Errorf("unknown policy type: %q", cfg.Type)
	}
}

func buildAndPolicy(settings component.TelemetrySettings, cfg *AndCfg) (Evaluator, error) {
	subs := make([]Evaluator, len(cfg.SubPolicyCfg))
	for i := range cfg.SubPolicyCfg {
		ev, err := buildShared(settings, &cfg.SubPolicyCfg[i].SharedPolicyCfg)
		if err != nil {
			return nil, fmt.Errorf("and sub-policy[%d]: %w", i, err)
		}
		subs[i] = ev
	}
	return NewAnd(settings.Logger, subs), nil
}

func buildNotPolicy(settings component.TelemetrySettings, cfg *NotCfg) (Evaluator, error) {
	sub, err := buildShared(settings, &cfg.SubPolicy.SharedPolicyCfg)
	if err != nil {
		return nil, fmt.Errorf("not sub-policy: %w", err)
	}
	return NewNot(settings.Logger, sub), nil
}

func buildDropPolicy(settings component.TelemetrySettings, cfg *DropCfg) (Evaluator, error) {
	subs := make([]Evaluator, len(cfg.SubPolicyCfg))
	for i := range cfg.SubPolicyCfg {
		ev, err := buildShared(settings, &cfg.SubPolicyCfg[i].SharedPolicyCfg)
		if err != nil {
			return nil, fmt.Errorf("drop sub-policy[%d]: %w", i, err)
		}
		subs[i] = ev
	}
	return NewDrop(settings.Logger, subs), nil
}

