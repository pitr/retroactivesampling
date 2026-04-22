package evaluator

import (
	"fmt"
	"regexp"

	"github.com/golang/groupcache/lru"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

const defaultCacheSize = 128

type stringAttributeFilter struct {
	key     string
	logger  *zap.Logger
	matcher func(string) bool
}

func NewStringAttributeFilter(logger *zap.Logger, key string, values []string, regexMatchEnabled bool, evictSize int) (Evaluator, error) {
	if regexMatchEnabled {
		if evictSize <= 0 {
			evictSize = defaultCacheSize
		}
		filterList, err := compileFilters(values)
		if err != nil {
			return nil, err
		}
		cache := lru.New(evictSize)
		return &stringAttributeFilter{
			key:    key,
			logger: logger,
			matcher: func(s string) bool {
				if v, ok := cache.Get(s); ok {
					return v.(bool)
				}
				for _, r := range filterList {
					if r.MatchString(s) {
						cache.Add(s, true)
						return true
					}
				}
				cache.Add(s, false)
				return false
			},
		}, nil
	}
	valuesMap := make(map[string]struct{})
	for _, v := range values {
		if v != "" {
			valuesMap[v] = struct{}{}
		}
	}
	return &stringAttributeFilter{
		key:     key,
		logger:  logger,
		matcher: func(s string) bool { _, ok := valuesMap[s]; return ok },
	}, nil
}

func (saf *stringAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	saf.logger.Debug("Evaluating spans in string-tag filter")
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			v, ok := r.Attributes().Get(saf.key)
			return ok && saf.matcher(v.Str())
		},
		func(span ptrace.Span) bool {
			v, ok := span.Attributes().Get(saf.key)
			return ok && v.Str() != "" && saf.matcher(v.Str())
		},
	), nil
}

func compileFilters(exprs []string) ([]*regexp.Regexp, error) {
	list := make([]*regexp.Regexp, 0, len(exprs))
	for _, e := range exprs {
		r, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("invalid regex `%s`: %w", e, err)
		}
		list = append(list, r)
	}
	return list, nil
}
