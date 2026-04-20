package processor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
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
		MaxInterestCacheEntries: 100_000,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set otelprocessor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (otelprocessor.Traces, error) {
	p, err := newProcessor(set.TelemetrySettings, cfg.(*Config), next)
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, p.processTraces,
		processorhelper.WithShutdown(p.Shutdown),
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
	)
}
