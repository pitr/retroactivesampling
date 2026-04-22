//go:build ignore

package evaluator

import (
	"fmt"
	"time"
)

type RuleConfig struct {
	Type      string        `mapstructure:"type"`
	Threshold time.Duration `mapstructure:"threshold"`
}

func Build(rules []RuleConfig) (Chain, error) {
	chain := make(Chain, 0, len(rules))
	for _, r := range rules {
		switch r.Type {
		case "error_status":
			chain = append(chain, &ErrorEvaluator{})
		case "high_latency":
			if r.Threshold == 0 {
				return nil, fmt.Errorf("high_latency rule requires threshold")
			}
			chain = append(chain, &LatencyEvaluator{Threshold: r.Threshold})
		default:
			return nil, fmt.Errorf("unknown rule type: %s", r.Type)
		}
	}
	return chain, nil
}
