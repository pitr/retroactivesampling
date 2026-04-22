package evaluator

import (
	"hash/fnv"
	"math"
	"math/big"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

const defaultHashSalt = "default-hash-seed"

type probabilisticSampler struct {
	logger    *zap.Logger
	threshold uint64
	hashSalt  string
}

func NewProbabilisticSampler(logger *zap.Logger, hashSalt string, samplingPercentage float64) Evaluator {
	if hashSalt == "" {
		hashSalt = defaultHashSalt
	}
	return &probabilisticSampler{
		logger:    logger,
		threshold: calculateThreshold(samplingPercentage / 100),
		hashSalt:  hashSalt,
	}
}

// Evaluate extracts trace ID from the first span (guaranteed present by groupByTrace)
// and returns SampledLocal — coordinator broadcast is skipped since all collectors
// with the same config make the same decision for a given trace ID.
func (s *probabilisticSampler) Evaluate(t ptrace.Traces) (Decision, error) {
	s.logger.Debug("Evaluating spans in probabilistic filter")
	rs := t.ResourceSpans()
	if rs.Len() == 0 {
		return NotSampled, nil
	}
	ss := rs.At(0).ScopeSpans()
	if ss.Len() == 0 {
		return NotSampled, nil
	}
	spans := ss.At(0).Spans()
	if spans.Len() == 0 {
		return NotSampled, nil
	}
	tid := spans.At(0).TraceID()
	if hashTraceID(s.hashSalt, tid[:]) <= s.threshold {
		return SampledLocal, nil
	}
	return NotSampled, nil
}

func calculateThreshold(ratio float64) uint64 {
	boundary := new(big.Float).SetInt(new(big.Int).SetUint64(math.MaxUint64))
	res, _ := boundary.Mul(boundary, big.NewFloat(ratio)).Uint64()
	return res
}

func hashTraceID(salt string, b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write(b)
	return h.Sum64()
}
