package prometheus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opsagent/internal/prometheus"
)

// matrixValue 构造一个 Prometheus matrix 响应。
func matrixValue(pod, container string, values [][2]interface{}) map[string]interface{} {
	result := []map[string]interface{}{{
		"metric": map[string]string{"pod": pod, "container": container},
		"values": values,
	}}
	return map[string]interface{}{
		"status": "success",
		"data":   map[string]interface{}{"resultType": "matrix", "result": result},
	}
}

func newPromServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryContainerMetrics(t *testing.T) {
	srv := newPromServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		query := r.FormValue("query")
		var payload map[string]interface{}
		switch {
		case strings.Contains(query, "working_set"):
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "90"},
				{float64(1728441660), "95"},
			})
		case strings.Contains(query, "resource_limits"):
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "100"},
			})
		case strings.Contains(query, "throttled_periods"):
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "0.5"},
			})
		default: // cpu_usage
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "0.2"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	})

	c, err := prometheus.New(prometheus.Config{BaseURL: srv.URL, QueryTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}

	got, err := c.Query(context.Background(), "default", []string{"p1"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	m := got["p1"]["app"]
	if m == nil {
		t.Fatalf("未按 pod/container 归并指标: %v", got)
	}
	if len(m.MemoryWorkingSet) != 2 {
		t.Errorf("内存工作集应有 2 个点，实际 %d", len(m.MemoryWorkingSet))
	}
	if m.MemoryLimit == nil || *m.MemoryLimit != 100 {
		t.Errorf("内存 limit 解析错误: %v", m.MemoryLimit)
	}
	if m.MemoryPeakRatio < 0.94 || m.MemoryPeakRatio > 0.96 {
		t.Errorf("峰值占比应约 0.95，实际 %f", m.MemoryPeakRatio)
	}
	if len(m.CPUUsageCores) != 1 || m.CPUUsageCores[0].V != 0.2 {
		t.Errorf("CPU 使用量解析错误: %v", m.CPUUsageCores)
	}
	if len(m.CPUThrottledRatio) != 1 || m.CPUThrottledRatio[0].V != 0.5 {
		t.Errorf("限流比率解析错误: %v", m.CPUThrottledRatio)
	}
}

func TestQueryPromQLHasSelectors(t *testing.T) {
	const ns = "production"
	pods := []string{"pod-a", "pod-b"}
	cases := []struct {
		name string
		ql   string
	}{
		{"working_set", prometheus.MemoryWorkingSetPromQL(ns, pods)},
		{"limits", prometheus.MemoryLimitPromQL(ns, pods)},
		{"cpu_usage", prometheus.CPUUsagePromQL(ns, pods)},
		{"throttle", prometheus.CPUThrottlePromQL(ns, pods)},
	}
	for _, tc := range cases {
		if !strings.Contains(tc.ql, fmt.Sprintf("%q", ns)) {
			t.Errorf("%s PromQL 缺少 namespace 过滤: %s", tc.name, tc.ql)
		}
		if !strings.Contains(tc.ql, "pod-a") || !strings.Contains(tc.ql, "pod-b") {
			t.Errorf("%s PromQL 缺少 pod 过滤: %s", tc.name, tc.ql)
		}
	}
}

func TestQueryAllFailReturnsError(t *testing.T) {
	srv := newPromServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c, _ := prometheus.New(prometheus.Config{BaseURL: srv.URL, QueryTimeout: time.Second})
	_, err := c.Query(context.Background(), "default", []string{"p1"}, time.Minute)
	if err == nil {
		t.Errorf("所有查询失败时应返回错误")
	}
}
