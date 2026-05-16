// Package otelinit configure le SDK OpenTelemetry pour l'app demo.
//
// - Exporter OTLP/gRPC vers Tempo (var d'env OTEL_EXPORTER_OTLP_ENDPOINT).
// - Resource attributes : service.name, service.version, deployment.environment.
// - Propagator W3C tracecontext + baggage (par défaut OTel mais explicité ici).
//
// Si OTEL_EXPORTER_OTLP_ENDPOINT est vide, on installe un tracer no-op : pas
// d'export, mais l'instrumentation reste fonctionnelle (les spans existent
// localement pour les tests in-memory).
package otelinit

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
}

// Setup installe le tracer provider global. Retourne un shutdown à appeler
// avant fin du process.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = os.Getenv("APP_VERSION")
	}
	if cfg.Environment == "" {
		cfg.Environment = "dev"
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironmentName(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// Pas d'endpoint : on installe quand même un provider avec resource,
		// mais sans exporter. Permet aux tests d'utiliser un sdktrace propre
		// via tracetest.NewInMemoryExporter par-dessus.
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		return tp.Shutdown, nil
	}

	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxQueueSize(2048),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func sampleRatio() float64 {
	switch os.Getenv("OTEL_TRACES_SAMPLER") {
	case "always_on", "":
		return 1.0
	case "always_off":
		return 0.0
	}
	// Géré par ailleurs par l'env OTEL_TRACES_SAMPLER_ARG via le SDK.
	return 1.0
}
