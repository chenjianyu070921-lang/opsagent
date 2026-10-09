package observability_test

import (
	"context"
	"testing"
	"time"

	"opsagent/internal/observability"
)

func TestInitNoEndpointIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := observability.Init(context.Background())
	if err != nil {
		t.Fatalf("未配置 endpoint 不应报错: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown 不应报错: %v", err)
	}
}

func TestInitWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_SERVICE_NAME", "opsagent-test")

	shutdown, err := observability.Init(context.Background())
	if err != nil {
		t.Fatalf("配置 endpoint 后初始化失败: %v", err)
	}

	// Tracer 应可用；产生一个 span（导出失败由 SDK 静默处理）。
	_, span := observability.Tracer().Start(context.Background(), observability.SpanCollect)
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown 失败: %v", err)
	}
}
