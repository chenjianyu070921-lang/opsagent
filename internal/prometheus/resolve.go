package prometheus

import (
	"context"
	"errors"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
)

// ErrPrometheusNotFound 表示三级地址解析都未命中。
var ErrPrometheusNotFound = errors.New("未找到 Prometheus（可用 --prometheus-url 或 OPSAGENT_PROMETHEUS_URL 指定）")

// 环境变量名。
const (
	EnvPrometheusURL = "OPSAGENT_PROMETHEUS_URL"
)

// ResolveURL 按优先级解析 Prometheus 地址：
// explicit flag → OPSAGENT_PROMETHEUS_URL → 集群内自动探测。
// 返回的 source 为 flag / env / discovery，用于报告与审计。
func ResolveURL(ctx context.Context, explicit string, k8s kubernetes.Interface) (baseURL, source string, err error) {
	if explicit != "" {
		return strings.TrimRight(explicit, "/"), "flag", nil
	}
	if env := os.Getenv(EnvPrometheusURL); env != "" {
		return strings.TrimRight(env, "/"), "env", nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return "", "", ErrPrometheusNotFound
	}
	url, err := Discover(ctx, k8s)
	if err != nil {
		return "", "", err
	}
	return url, "discovery", nil
}
