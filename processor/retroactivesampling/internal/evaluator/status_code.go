package evaluator

import (
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type statusCodeFilter struct {
	logger      *zap.Logger
	statusCodes []ptrace.StatusCode
}

func NewStatusCodeFilter(logger *zap.Logger, statusCodeStrings []string) (Evaluator, error) {
	if len(statusCodeStrings) == 0 {
		return nil, errors.New("expected at least one status code to filter on")
	}
	codes := make([]ptrace.StatusCode, len(statusCodeStrings))
	for i, s := range statusCodeStrings {
		switch s {
		case "OK":
			codes[i] = ptrace.StatusCodeOk
		case "ERROR":
			codes[i] = ptrace.StatusCodeError
		case "UNSET":
			codes[i] = ptrace.StatusCodeUnset
		default:
			return nil, fmt.Errorf("unknown status code %q, supported: OK, ERROR, UNSET", s)
		}
	}
	return &statusCodeFilter{logger: logger, statusCodes: codes}, nil
}

func (r *statusCodeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	r.logger.Debug("Evaluating spans in status code filter")
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		for _, c := range r.statusCodes {
			if span.Status().Code() == c {
				return true
			}
		}
		return false
	}), nil
}
