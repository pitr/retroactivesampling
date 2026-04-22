package evaluator

import (
	"encoding/binary"
	"hash/fnv"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

const (
	numHashBuckets        = 0x4000 // 16384 = 2^14, matches probabilisticsamplerprocessor
	percentageScaleFactor = numHashBuckets / 100.0
)

type probabilisticSampler struct {
	logger    *zap.Logger
	threshold uint32
	hashSeed  uint32
}

func NewProbabilisticSampler(logger *zap.Logger, hashSeed uint32, samplingPercentage float64) Evaluator {
	return &probabilisticSampler{
		logger:    logger,
		threshold: uint32(samplingPercentage * percentageScaleFactor),
		hashSeed:  hashSeed,
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
	if hashTraceID(s.hashSeed, tid[:]) < s.threshold {
		return SampledLocal, nil
	}
	return NotSampled, nil
}

func hashTraceID(seed uint32, b []byte) uint32 {
	h := fnv.New32a()
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], seed)
	_, _ = h.Write(buf[:])
	_, _ = h.Write(b)
	return h.Sum32() & (numHashBuckets - 1)
}
