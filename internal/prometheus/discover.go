package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BuildInfoProbe 探测候选地址是否为可用 Prometheus（/api/v1/status/buildinfo 返回 2xx）。
// 声明为包级变量，测试时替换为假探针，避免依赖集群 DNS。
var BuildInfoProbe = func(ctx context.Context, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/status/buildinfo", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// 探测参数：候选命名空间、候选数上限、总预算。
var (
	discoveryNamespaces = []string{"monitoring", "observability", "default", "kube-system"}
	maxCandidates       = 8
	discoveryTimeout    = 3 * time.Second
)

// candidate 是一个待探测的 Service 地址。
type candidate struct {
	baseURL string
}

// Discover 在候选命名空间枚举 Prometheus Service 并并行探测，首个成功者胜出。
func Discover(ctx context.Context, k8s kubernetes.Interface) (string, error) {
	candidates := listCandidates(ctx, k8s)
	if len(candidates) == 0 {
		return "", ErrPrometheusNotFound
	}

	probeCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	result := make(chan string, 1)
	for _, c := range candidates {
		go func(baseURL string) {
			if BuildInfoProbe(probeCtx, baseURL) {
				select {
				case result <- baseURL:
				default:
				}
			}
		}(c.baseURL)
	}

	select {
	case url := <-result:
		return url, nil
	case <-probeCtx.Done():
		return "", ErrPrometheusNotFound
	}
}

// listCandidates 枚举候选 Service：先按名字子串、再按标准标签兜底，按 URL 去重并截断到上限。
func listCandidates(ctx context.Context, k8s kubernetes.Interface) []candidate {
	seen := map[string]bool{}
	var out []candidate

	add := func(svc *corev1.Service) {
		port := servicePrometheusPort(svc)
		if port == 0 {
			return
		}
		baseURL := fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, port)
		if seen[baseURL] {
			return
		}
		seen[baseURL] = true
		out = append(out, candidate{baseURL: baseURL})
	}

	for _, ns := range discoveryNamespaces {
		services, err := k8s.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range services.Items {
			svc := &services.Items[i]
			if strings.Contains(svc.Name, "prometheus") {
				add(svc)
			}
		}
	}

	// 标签兜底：app.kubernetes.io/name=prometheus
	if len(out) == 0 {
		for _, ns := range discoveryNamespaces {
			services, err := k8s.CoreV1().Services(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=prometheus",
			})
			if err != nil {
				continue
			}
			for i := range services.Items {
				add(&services.Items[i])
			}
		}
	}

	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// servicePrometheusPort 选择 Prometheus 端口：优先名为 web/http 的 9090 端口，
// 否则取第一个 TCP 端口。
func servicePrometheusPort(svc *corev1.Service) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Port == 9090 {
			return p.Port
		}
	}
	for _, p := range svc.Spec.Ports {
		if p.Protocol == corev1.ProtocolTCP {
			return p.Port
		}
	}
	return 0
}
