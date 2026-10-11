package prometheus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Config 是 Prometheus 客户端配置。
type Config struct {
	BaseURL      string
	QueryTimeout time.Duration // 0 时默认 10s
}

// Client 通过官方 HTTP API 客户端执行固定的 4 条范围查询。
type Client struct {
	api     v1.API
	baseURL string
	timeout time.Duration
}

// New 创建客户端并校验地址格式。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("Prometheus 地址为空")
	}
	promClient, err := api.NewClient(api.Config{Address: cfg.BaseURL})
	if err != nil {
		return nil, fmt.Errorf("创建 Prometheus HTTP 客户端失败: %w", err)
	}
	timeout := cfg.QueryTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		api:     v1.NewAPI(promClient),
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		timeout: timeout,
	}, nil
}

// BaseURL 返回本客户端连接的 Prometheus 地址。
func (c *Client) BaseURL() string { return c.baseURL }

// namedQuery 是一条命名的固定查询。
type namedQuery struct {
	name string
	ql   string
}

// Query 并发执行 4 条范围查询并按 pod/container 归并；
// 单条查询失败不影响其他查询，只有 4 条全失败时才返回错误。
func (c *Client) Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*ContainerMetrics, error) {
	end := time.Now()
	start := end.Add(-window)
	step := window / 60
	if step < 15*time.Second {
		step = 15 * time.Second
	}
	queryRange := v1.Range{Start: start, End: end, Step: step}

	queries := []namedQuery{
		{"working_set", MemoryWorkingSetPromQL(namespace, podNames)},
		{"memory_limit", MemoryLimitPromQL(namespace, podNames)},
		{"cpu_usage", CPUUsagePromQL(namespace, podNames)},
		{"cpu_throttle", CPUThrottlePromQL(namespace, podNames)},
	}

	type matrixResult struct {
		name   string
		matrix model.Matrix
		err    error
	}
	results := make([]matrixResult, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q namedQuery) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			value, _, _, err := c.api.QueryRange(qctx, q.ql, queryRange)
			if err != nil {
				results[i] = matrixResult{name: q.name, err: err}
				return
			}
			matrix, ok := value.(model.Matrix)
			if !ok {
				results[i] = matrixResult{name: q.name, err: fmt.Errorf("%s 返回类型不是 matrix", q.name)}
				return
			}
			results[i] = matrixResult{name: q.name, matrix: matrix}
		}(i, q)
	}
	wg.Wait()

	metrics := map[string]map[string]*ContainerMetrics{}
	get := func(pod, container string) *ContainerMetrics {
		if metrics[pod] == nil {
			metrics[pod] = map[string]*ContainerMetrics{}
		}
		m := metrics[pod][container]
		if m == nil {
			m = &ContainerMetrics{Pod: pod, Container: container}
			metrics[pod][container] = m
		}
		return m
	}

	var failures []string
	for _, res := range results {
		if res.err != nil {
			failures = append(failures, res.name+": "+res.err.Error())
			continue
		}
		applyMatrix(res.name, res.matrix, get)
	}
	finalizePeaks(metrics)
	if len(failures) == len(queries) {
		return nil, fmt.Errorf("所有 Prometheus 查询均失败: %s", strings.Join(failures, "; "))
	}
	return metrics, nil
}

// applyMatrix 把一条查询的 matrix 结果写入对应容器指标。
func applyMatrix(queryName string, matrix model.Matrix, get func(pod, container string) *ContainerMetrics) {
	for _, ss := range matrix {
		pod := string(ss.Metric["pod"])
		container := string(ss.Metric["container"])
		if pod == "" || container == "" {
			continue
		}
		m := get(pod, container)
		points := make([]Point, 0, len(ss.Values))
		for _, sp := range ss.Values {
			points = append(points, Point{T: sp.Timestamp.Time(), V: float64(sp.Value)})
		}
		switch queryName {
		case "working_set":
			m.MemoryWorkingSet = points
		case "memory_limit":
			if len(points) > 0 {
				v := points[len(points)-1].V
				m.MemoryLimit = &v
			}
		case "cpu_usage":
			m.CPUUsageCores = points
		case "cpu_throttle":
			m.CPUThrottledRatio = points
		}
	}
}

// finalizePeaks 为每个容器计算内存工作集峰值相对 limit 的占比。
func finalizePeaks(metrics map[string]map[string]*ContainerMetrics) {
	for _, containers := range metrics {
		for _, m := range containers {
			if m.MemoryLimit == nil || *m.MemoryLimit <= 0 {
				continue
			}
			peak := 0.0
			for _, p := range m.MemoryWorkingSet {
				if p.V > peak {
					peak = p.V
				}
			}
			m.MemoryPeakRatio = peak / *m.MemoryLimit
		}
	}
}
