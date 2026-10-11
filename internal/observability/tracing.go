// Package observability 负责 opsagent 自身的可观测性：OpenTelemetry trace 初始化。
// 未配置 OTLP endpoint 时完全 noop：不启动 goroutine、不发起连接。
package observability

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// 各阶段 span 名。
const (
	SpanDiagnose   = "diagnose"
	SpanCollect    = "collect"
	SpanPrometheus = "prometheus"
	SpanRules      = "rules"
	SpanLLM        = "llm"
	SpanBuild      = "build"
	SpanPersist    = "persist"
)

// Init 按标准 OTEL 环境变量初始化 trace；无 endpoint 时返回 noop shutdown。
func Init(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	// exporter 自动读取 OTEL_EXPORTER_OTLP_ENDPOINT / HEADERS 等标准变量。
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("创建 OTLP HTTP exporter 失败: %w", err)
	}

	// WithFromEnv 读取 OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES，
	// 放在默认服务名之后以覆盖默认值。
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("opsagent")),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("创建追踪资源失败: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return func(ctx context.Context) error {
		return tracerProvider.Shutdown(ctx)
	}, nil
}

// Tracer 返回全局 tracer。
func Tracer() trace.Tracer {
	return otel.Tracer("opsagent")
}
