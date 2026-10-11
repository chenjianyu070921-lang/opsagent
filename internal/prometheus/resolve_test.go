package prometheus_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"opsagent/internal/prometheus"
)

func TestResolveURLFlagAndEnv(t *testing.T) {
	k8s := fake.NewSimpleClientset()

	got, source, err := prometheus.ResolveURL(context.Background(), "http://flag:9090/", k8s)
	if err != nil || got != "http://flag:9090" || source != "flag" {
		t.Errorf("flag 优先级解析错误: %q %q %v", got, source, err)
	}

	t.Setenv("OPSAGENT_PROMETHEUS_URL", "http://env:9090")
	got, source, err = prometheus.ResolveURL(context.Background(), "", k8s)
	if err != nil || got != "http://env:9090" || source != "env" {
		t.Errorf("env 解析错误: %q %q %v", got, source, err)
	}
}

func TestResolveURLSkipsDiscoveryOutsideCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "") // 集群外：不探测
	k8s := fake.NewSimpleClientset()
	_, _, err := prometheus.ResolveURL(context.Background(), "", k8s)
	if !errors.Is(err, prometheus.ErrPrometheusNotFound) {
		t.Errorf("集群外未配置时应返回 ErrPrometheusNotFound，实际 %v", err)
	}
}

func TestDiscoverFindsService(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "monitoring",
			Name:      "prometheus-k8s",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "web", Port: 9090, Protocol: corev1.ProtocolTCP}},
		},
	}
	k8s := fake.NewSimpleClientset(svc)

	// 用假探针避免真实 DNS 解析。
	old := prometheus.BuildInfoProbe
	prometheus.BuildInfoProbe = func(ctx context.Context, baseURL string) bool {
		return baseURL == "http://prometheus-k8s.monitoring.svc:9090"
	}
	t.Cleanup(func() { prometheus.BuildInfoProbe = old })

	got, err := prometheus.Discover(context.Background(), k8s)
	if err != nil {
		t.Fatalf("应探测到 Prometheus Service: %v", err)
	}
	if got != "http://prometheus-k8s.monitoring.svc:9090" {
		t.Errorf("探测地址错误: %q", got)
	}
}

func TestDiscoverNoService(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	old := prometheus.BuildInfoProbe
	prometheus.BuildInfoProbe = func(ctx context.Context, baseURL string) bool { return false }
	t.Cleanup(func() { prometheus.BuildInfoProbe = old })

	_, err := prometheus.Discover(context.Background(), fake.NewSimpleClientset())
	if !errors.Is(err, prometheus.ErrPrometheusNotFound) {
		t.Errorf("无候选服务时应返回 ErrPrometheusNotFound，实际 %v", err)
	}
}
