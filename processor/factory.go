package processor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
)

const typeStr = "retroactive_sampling"

func NewFactory() otelprocessor.Factory {
	return otelprocessor.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		otelprocessor.WithTraces(createTracesProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		BufferTTL:        10 * time.Second,
		DropTTL:          30 * time.Second,
		InterestCacheTTL: 60 * time.Second,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set otelprocessor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (otelprocessor.Traces, error) {
	return newProcessor(set.Logger, cfg.(*Config), next)
}
