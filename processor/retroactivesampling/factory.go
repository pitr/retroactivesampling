package retroactivesampling

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/metadata"
)

func NewFactory() otelprocessor.Factory {
	return otelprocessor.NewFactory(
		metadata.Type,
		createDefaultConfig,
		otelprocessor.WithTraces(createTracesProcessor, metadata.TracesStability),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		DecisionWaitTime: 30 * time.Second,
		ChunkSize:        4096,
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
		processorhelper.WithStart(p.Start),
		processorhelper.WithShutdown(p.Shutdown),
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
	)
}
