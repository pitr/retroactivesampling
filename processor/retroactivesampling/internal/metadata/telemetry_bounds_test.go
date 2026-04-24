package metadata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var wantSpanAgeBounds = []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

func TestSpanAgeOnEvictionHistogramIsInt64WithCorrectBounds(t *testing.T) {
	testTel := componenttest.NewTelemetry()
	tb, err := NewTelemetryBuilder(testTel.NewTelemetrySettings())
	require.NoError(t, err)
	defer tb.Shutdown()

	tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), 1)

	got, err := testTel.GetMetric("otelcol_retroactive_sampling_buffer_span_age_on_eviction")
	require.NoError(t, err)

	hist, ok := got.Data.(metricdata.Histogram[int64])
	require.True(t, ok, "expected int64 histogram, got %T", got.Data)
	require.Len(t, hist.DataPoints, 1)
	require.Equal(t, wantSpanAgeBounds, hist.DataPoints[0].Bounds)

	require.NoError(t, testTel.Shutdown(context.Background()))
}

func TestLiveBytesGaugeIsInt64(t *testing.T) {
	testTel := componenttest.NewTelemetry()
	tb, err := NewTelemetryBuilder(testTel.NewTelemetrySettings())
	require.NoError(t, err)
	defer tb.Shutdown()

	err = tb.RegisterRetroactiveSamplingBufferLiveBytesCallback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(42)
		return nil
	})
	require.NoError(t, err)

	got, err := testTel.GetMetric("otelcol_retroactive_sampling_buffer_live_bytes")
	require.NoError(t, err)

	gauge, ok := got.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "expected int64 gauge, got %T", got.Data)
	require.Len(t, gauge.DataPoints, 1)
	assert.Equal(t, int64(42), gauge.DataPoints[0].Value)
	require.NoError(t, testTel.Shutdown(context.Background()))
}

func TestOrphanedBytesGaugeIsInt64(t *testing.T) {
	testTel := componenttest.NewTelemetry()
	tb, err := NewTelemetryBuilder(testTel.NewTelemetrySettings())
	require.NoError(t, err)
	defer tb.Shutdown()

	err = tb.RegisterRetroactiveSamplingBufferOrphanedBytesCallback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(99)
		return nil
	})
	require.NoError(t, err)

	got, err := testTel.GetMetric("otelcol_retroactive_sampling_buffer_orphaned_bytes")
	require.NoError(t, err)

	gauge, ok := got.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "expected int64 gauge, got %T", got.Data)
	require.Len(t, gauge.DataPoints, 1)
	assert.Equal(t, int64(99), gauge.DataPoints[0].Value)
	require.NoError(t, testTel.Shutdown(context.Background()))
}
