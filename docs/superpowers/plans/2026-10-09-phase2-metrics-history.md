# Phase 2：指标和历史记录 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 opsagent 增加 Prometheus 指标证据、SQLite 诊断历史、证据引用增强、OpenTelemetry 自身追踪和 Markdown/HTML 报告导出。

**Architecture:** 编排从 Cobra 移到 `internal/agent` 的固定阶段管线（collect→metrics→rules→llm→build→persist）；`report.Document` 是渲染器与历史存储共同的唯一数据模型；Prometheus、SQLite 等外部系统收敛在边界接口后，失败统一降级为 notice，规则报告永远可产出。

**Tech Stack:** Go 1.26、Cobra、client-go、prometheus/client_golang/api、glebarez/go-sqlite（modernc 纯 Go，零 CGO）、OpenTelemetry OTLP/HTTP、goldmark、html/template + go:embed。

## Global Constraints

- Go module `opsagent`，`go 1.26.0`；用户可见文案一律中文。
- 零 CGO：SQLite 必须用 `github.com/glebarez/go-sqlite`（driver 名 `sqlite`），禁止 mattn/go-sqlite3。
- 默认只读：不新增任何 Kubernetes 写操作。
- DB 默认路径 `os.UserConfigDir()/opsagent/history.db`，可用 `--history-db` / `OPSAGENT_HISTORY_DB` 覆盖。
- 指标窗口默认 30m；Prometheus 查询整体超时 10s；集群内探测总预算 3s；OTel shutdown flush 预算 5s。
- 规则阈值：内存峰值/limit ≥ 85% 报内存压力；CPU 限流比率均值 ≥ 25% 报限流。
- ev-x 编号只在单次诊断内有效；历史冻结编号原样复现。
- 全部测试离线可跑，门禁 `go test ./...`；每个任务结束必须提交。

## File Structure

```text
internal/prometheus/
  types.go      Point / ContainerMetrics / Querier 接口
  links.go      GraphURL / GrafanaExploreURL
  queries.go    4 条 PromQL 构造
  client.go     Client（实现 Querier）
  resolve.go    ResolveURL（flag/env/探测）
  discover.go   集群内 Service 枚举与 buildinfo 探测
internal/diagnose/
  snapshot.go   修改：Snapshot 增 Metrics 等字段；Evidence 增 Timestamp/Links；AddWith
  rules.go      修改：RunRules 增时间戳回填与 OOM 去重
  metrics_rules.go  新增：memoryTrendRule / cpuThrottleRule 及辅助函数
internal/report/
  document.go   Document 等渲染无关模型
  builder.go    BuildDocument / ValidateCitations / NeutralizeCitations
  renderer.go   Renderer 接口 / For / FormatFromPath
  markdown.go   修改：改为消费 Document
  html.go       HTMLRenderer
  report.html   内嵌 HTML 模板
internal/storage/
  sqlite.go     Open / DefaultPath
  migrate.go    PRAGMA user_version 迁移
  repository.go Repository 接口与 Record
  sqlite_repository.go Repository 的 SQLite 实现
internal/observability/
  tracing.go    Init / Tracer / span 名常量
internal/agent/
  runner.go     Deps / Input / Output / Runner.Run
internal/cli/
  root.go       修改：Execute(ctx)、注册 --history-db
  diagnose.go   修改：瘦身为装配 + 渲染；新 flags；--last 分支
  history.go    新增：history list/show/export
cmd/opsagent/
  main.go       修改：signal context + tracing Init/Shutdown
```

依赖顺序：Task 1→2→3（prometheus 叶子包）→4→5（diagnose）→6→7→8（report）→9→10（storage）→11（observability）→12（agent）→13→14（cli）→15（main + README）。

---

### Task 1: Prometheus 指标类型与证据链接

**Files:**
- Create: `internal/prometheus/types.go`
- Create: `internal/prometheus/links.go`
- Test: `internal/prometheus/links_test.go`

**Interfaces:**
- Produces:
  - `type Point struct { T time.Time; V float64 }`
  - `type ContainerMetrics struct { Pod, Container string; MemoryWorkingSet []Point; MemoryLimit *float64; MemoryPeakRatio float64; CPUUsageCores []Point; CPULimit *float64; CPUThrottledRatio []Point }`
  - `type Querier interface { Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*ContainerMetrics, error) }`
  - `func GraphURL(base, promQL string, start, end time.Time) string`
  - `func GrafanaExploreURL(grafanaBase, promQL string, start, end time.Time) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/prometheus/links_test.go`：

```go
package prometheus_test

import (
	"net/url"
	"testing"
	"time"

	"opsagent/internal/prometheus"
)

func TestGraphURL(t *testing.T) {
	start := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	got := prometheus.GraphURL("http://prom:9090", "container_memory_working_set_bytes{namespace=\"default\"}", start, end)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("返回值不是合法 URL: %v", err)
	}
	if u.Path != "/graph" {
		t.Errorf("路径应为 /graph，实际 %s", u.Path)
	}
	q := u.Query()
	if q.Get("g0.expr") != "container_memory_working_set_bytes{namespace=\"default\"}" {
		t.Errorf("PromQL 未完整编码: %q", q.Get("g0.expr"))
	}
	if q.Get("g0.tab") != "0" || q.Get("g0.start") == "" || q.Get("g0.end") == "" {
		t.Errorf("graph 参数缺失: %v", q)
	}
}

func TestGrafanaExploreURL(t *testing.T) {
	start := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)

	if got := prometheus.GrafanaExploreURL("", "up", start, end); got != "" {
		t.Errorf("grafana 地址为空时应返回空串，实际 %q", got)
	}

	got := prometheus.GrafanaExploreURL("http://grafana:3000", "up{namespace=\"default\"}", start, end)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("返回值不是合法 URL: %v", err)
	}
	if u.Path != "/explore" {
		t.Errorf("路径应为 /explore，实际 %s", u.Path)
	}
	left := u.Query().Get("left")
	if left == "" || !contains(left, "up{namespace=\"default\"}") {
		t.Errorf("left 参数缺少 PromQL: %q", left)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/prometheus/`
Expected: FAIL（包不存在 / undefined: prometheus）

- [ ] **Step 3: 实现类型与链接函数**

创建 `internal/prometheus/types.go`：

```go
// Package prometheus 封装 Prometheus HTTP API：指标领域类型、固定查询、地址解析与证据链接。
// 本包是叶子包，不依赖任何 opsagent 内部包。
package prometheus

import (
	"context"
	"time"
)

// Point 是一个指标采样点。
type Point struct {
	T time.Time
	V float64
}

// ContainerMetrics 是单个容器在一个时间窗口内的内存与 CPU 指标。
// 序列为空表示该类指标未取到；Limit 为 nil 表示集群缺少 limit 指标。
type ContainerMetrics struct {
	Pod       string
	Container string
	// 内存工作集（cAdvisor working_set）与其 limit，以及窗口内峰值占比。
	MemoryWorkingSet []Point
	MemoryLimit      *float64
	MemoryPeakRatio  float64
	// CPU 使用量（核）、CPU limit、限流时间片比率序列。
	CPUUsageCores     []Point
	CPULimit          *float64
	CPUThrottledRatio []Point
}

// Querier 是指标查询的边界接口，真实实现与假实现对等可换。
type Querier interface {
	// Query 按命名空间和 Pod 名集合查询一个时间窗口的容器指标。
	// podNames 为空时不加 Pod 过滤。
	Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*ContainerMetrics, error)
}
```

创建 `internal/prometheus/links.go`：

```go
package prometheus

import (
	"fmt"
	"net/url"
	"time"
)

// GraphURL 生成 Prometheus 自带图表页链接，打开即可核对该 PromQL 的原始曲线。
func GraphURL(base, promQL string, start, end time.Time) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = "/graph"
	q := u.Query()
	q.Set("g0.expr", promQL)
	q.Set("g0.tab", "0")
	q.Set("g0.start", start.Format(time.RFC3339))
	q.Set("g0.end", end.Format(time.RFC3339))
	u.RawQuery = q.Encode()
	return u.String()
}

// GrafanaExploreURL 生成 Grafana Explore 链接；grafanaBase 为空时返回空串。
func GrafanaExploreURL(grafanaBase, promQL string, start, end time.Time) string {
	if grafanaBase == "" {
		return ""
	}
	u, err := url.Parse(grafanaBase)
	if err != nil {
		return ""
	}
	u.Path = "/explore"
	left := fmt.Sprintf(`{"datasource":"Prometheus","queries":[{"expr":%q,"refId":"A"}],"range":{"from":%q,"to":%q}}`,
		promQL, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	u.RawQuery = "left=" + url.QueryEscape(left)
	return u.String()
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/prometheus/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/prometheus/
git commit -m "feat(prometheus): 指标领域类型与证据链接生成"
```

---

### Task 2: Prometheus 客户端（4 条固定查询）

**Files:**
- Create: `internal/prometheus/queries.go`
- Create: `internal/prometheus/client.go`
- Test: `internal/prometheus/client_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Point`、`ContainerMetrics`、`Querier`
- Produces:
  - `type Config struct { BaseURL string; QueryTimeout time.Duration }`
  - `func New(cfg Config) (*Client, error)`
  - `func (c *Client) BaseURL() string`
  - `func MemoryWorkingSetPromQL(namespace string, podNames []string) string`
  - `func MemoryLimitPromQL(namespace string, podNames []string) string`
  - `func CPUUsagePromQL(namespace string, podNames []string) string`
  - `func CPUThrottlePromQL(namespace string, podNames []string) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/prometheus/client_test.go`：

```go
package prometheus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		case contains(query, "working_set"):
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "90"},
				{float64(1728441660), "95"},
			})
		case contains(query, "resource_limits"):
			payload = matrixValue("p1", "app", [][2]interface{}{
				{float64(1728441600), "100"},
			})
		case contains(query, "throttled_periods"):
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
		if !contains(tc.ql, fmt.Sprintf("%q", ns)) {
			t.Errorf("%s PromQL 缺少 namespace 过滤: %s", tc.name, tc.ql)
		}
		if !contains(tc.ql, "pod-a") || !contains(tc.ql, "pod-b") {
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/prometheus/`
Expected: FAIL（undefined: prometheus.New / MemoryWorkingSetPromQL 等）

- [ ] **Step 3: 实现 PromQL 构造**

创建 `internal/prometheus/queries.go`：

```go
package prometheus

import (
	"fmt"
	"regexp"
	"strings"
)

// podMatcher 把 Pod 名集合转成锚定的正则 label 选择器；名字经 QuoteMeta 转义，
// 防止名称里的特殊字符注入 PromQL。空集合返回空串。
func podMatcher(podNames []string) string {
	if len(podNames) == 0 {
		return ""
	}
	parts := make([]string, len(podNames))
	for i, name := range podNames {
		parts[i] = regexp.QuoteMeta(name)
	}
	return "pod=~\"(?:" + strings.Join(parts, "|") + ")\","
}

// MemoryWorkingSetPromQL 构造 cAdvisor 内存工作集查询。
func MemoryWorkingSetPromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`container_memory_working_set_bytes{namespace=%q,container!="",container!="POD",%s}`,
		namespace, podMatcher(podNames))
}

// MemoryLimitPromQL 构造 kube-state-metrics 的内存 limit 查询。
func MemoryLimitPromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`kube_pod_container_resource_limits{namespace=%q,resource="memory",%s}`,
		namespace, podMatcher(podNames))
}

// CPUUsagePromQL 构造 CPU 使用速率（核/秒）查询。
func CPUUsagePromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`rate(container_cpu_usage_seconds_total{namespace=%q,container!="",container!="POD",%s}[5m])`,
		namespace, podMatcher(podNames))
}

// CPUThrottlePromQL 构造 CFS 限流时间片占比查询。
func CPUThrottlePromQL(namespace string, podNames []string) string {
	matcher := podMatcher(podNames)
	return fmt.Sprintf(
		`rate(container_cpu_cfs_throttled_periods_total{namespace=%q,container!="",%s}[5m]) `+
			`/ rate(container_cpu_cfs_periods_total{namespace=%q,container!="",%s}[5m])`,
		namespace, matcher, namespace, matcher)
}
```

- [ ] **Step 4: 实现客户端**

创建 `internal/prometheus/client.go`：

```go
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
			value, _, err := c.api.QueryRange(qctx, q.ql, queryRange)
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

	// working_set 与 limit 都到位后计算窗口峰值占比。
	if queryName == "working_set" {
		for _, containers := range metricsByMatrix(matrix) {
			_ = containers
		}
	}
}

// metricsByMatrix 仅用于满足编译期占位，实际峰值在 finalizePeaks 中计算。
func metricsByMatrix(m model.Matrix) []string { return nil }
```

上面 `applyMatrix` 无法拿到全部容器做峰值计算。把 Step 4 末尾改为在 `Query` 归并完成后统一调用 `finalizePeaks(metrics)`，删除 `applyMatrix` 里的峰值占位和 `metricsByMatrix`：

在 `Query` 的 `if len(failures) == len(queries)` 之前插入：

```go
	finalizePeaks(metrics)
```

并在 `client.go` 末尾追加：

```go
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
```

同时删除 `applyMatrix` 中这一整段：

```go
	// working_set 与 limit 都到位后计算窗口峰值占比。
	if queryName == "working_set" {
		for _, containers := range metricsByMatrix(matrix) {
			_ = containers
		}
	}
```

以及整个 `metricsByMatrix` 函数。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/prometheus/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/prometheus/
git commit -m "feat(prometheus): 客户端并发查询内存与 CPU 指标"
```

---

### Task 3: Prometheus 地址解析与集群内探测

**Files:**
- Create: `internal/prometheus/resolve.go`
- Create: `internal/prometheus/discover.go`
- Test: `internal/prometheus/resolve_test.go`

**Interfaces:**
- Produces:
  - `var ErrPrometheusNotFound = errors.New(...)`
  - `func ResolveURL(ctx context.Context, explicit string, k8s kubernetes.Interface) (baseURL, source string, err error)`
  - `func Discover(ctx context.Context, k8s kubernetes.Interface) (string, error)`

- [ ] **Step 1: 写失败测试**

创建 `internal/prometheus/resolve_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/prometheus/`
Expected: FAIL（undefined: ResolveURL / Discover / BuildInfoProbe）

- [ ] **Step 3: 实现地址解析**

创建 `internal/prometheus/resolve.go`：

```go
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
```

- [ ] **Step 4: 实现集群内探测**

创建 `internal/prometheus/discover.go`：

```go
package prometheus

import (
	"context"
	"fmt"
	"net/http"
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
			if contains(svc.Name, "prometheus") {
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
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/prometheus/`
Expected: PASS

- [ ] **Step 6: 运行全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/prometheus/
git commit -m "feat(prometheus): flag/env 地址解析与集群内自动探测"
```

---

### Task 4: Snapshot/Evidence 模型升级与时间戳回填

**Files:**
- Modify: `internal/diagnose/snapshot.go`
- Modify: `internal/diagnose/rules.go`
- Test: `internal/diagnose/evidence_test.go`

**Interfaces:**
- Consumes: `opsagent/internal/prometheus.ContainerMetrics`
- Produces:
  - `type EvidenceLink struct { Type, Label, URL string }`
  - Evidence 新增字段 `Timestamp *time.Time`、`Links []EvidenceLink`
  - Snapshot 新增字段 `PrometheusURL string`、`GrafanaURL string`、`MetricsWindow time.Duration`、`Metrics map[string]map[string]*prometheus.ContainerMetrics`
  - `func NewEvidenceSet() *EvidenceSet`
  - `func (s *EvidenceSet) Add(kind, source, summary, detail string) string`（签名不变）
  - `func (s *EvidenceSet) AddWith(ev Evidence) string`
  - `RunRules` 返回的证据 `Timestamp` 一定非 nil

- [ ] **Step 1: 写失败测试**

创建 `internal/diagnose/evidence_test.go`：

```go
package diagnose_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
)

func TestEvidenceAddWith(t *testing.T) {
	set := diagnose.NewEvidenceSet()
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	id := set.AddWith(diagnose.Evidence{
		Kind:      "metric",
		Source:    "pod/p container/app",
		Summary:   "趋势",
		Timestamp: &ts,
		Links:     []diagnose.EvidenceLink{{Type: "graph", Label: "图表", URL: "http://prom/graph"}},
	})
	if id != "ev-1" {
		t.Errorf("第一个证据编号应为 ev-1，实际 %s", id)
	}
	id2 := set.Add("status", "pod/p", "状态", "细节")
	if id2 != "ev-2" {
		t.Errorf("Add 编号应为 ev-2，实际 %s", id2)
	}
	got := set.List()
	if len(got) != 2 || got[0].Timestamp == nil || len(got[0].Links) != 1 {
		t.Errorf("证据元数据未保留: %+v", got)
	}
}

func TestEventEvidenceTimestampFromEvent(t *testing.T) {
	eventTime := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p8",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name: "app", Ready: false,
		}},
	})
	snap.Events["p8"] = []kubernetes.WarningEvent{{
		Reason: "FailedScheduling", Message: "no eligible node", LastTimestamp: eventTime,
	}}

	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)

	var eventEv *diagnose.Evidence
	for i := range evidence {
		if evidence[i].Kind == "event" {
			eventEv = &evidence[i]
		}
	}
	if eventEv == nil {
		t.Fatalf("未找到事件证据")
	}
	if eventEv.Timestamp == nil || !eventEv.Timestamp.Equal(eventTime) {
		t.Errorf("事件证据时间戳应为事件 LastTimestamp，实际 %v", eventEv.Timestamp)
	}

	// 其余证据也必须有时间戳。
	for _, e := range evidence {
		if e.Timestamp == nil {
			t.Errorf("证据 %s 缺少时间戳", e.ID)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/diagnose/`
Expected: FAIL（undefined: AddWith / EvidenceLink；Timestamp nil）

- [ ] **Step 3: 升级 Snapshot 与 Evidence**

修改 `internal/diagnose/snapshot.go`，把 `Snapshot` 改为：

```go
// Snapshot 是一次诊断采集到的全部现场信息。
type Snapshot struct {
	Target     Target
	Deployment *kubernetes.DeploymentStatus
	// DeploymentEvents 是与 Deployment 自身相关的事件（FailedCreate、ExceededQuota 等）。
	DeploymentEvents []kubernetes.WarningEvent
	Pods             []kubernetes.PodStatus
	// Events 按 Pod 名索引该 Pod 的 Warning 事件。
	Events map[string][]kubernetes.WarningEvent
	// Logs 按 Pod 名 → 容器名索引可疑容器日志。
	Logs map[string]map[string]kubernetes.ContainerLog
	// Prometheus 指标阶段产生的现场：地址、Grafana 地址、查询窗口与按 pod/container 索引的指标。
	PrometheusURL string
	GrafanaURL    string
	MetricsWindow time.Duration
	Metrics       map[string]map[string]*prometheus.ContainerMetrics
	CollectedAt   time.Time
}
```

并在 import 块加入 `"opsagent/internal/prometheus"`。

把文件末尾的 Evidence/EvidenceSet 部分整体替换为：

```go
// EvidenceLink 是证据指向的外部系统链接。
type EvidenceLink struct {
	Type  string // graph / grafana / promql
	Label string
	URL   string
}

// Evidence 是一条可被结论引用的证据。
type Evidence struct {
	ID      string // ev-1、ev-2…，由 EvidenceSet 分配
	Kind    string // status / event / log / deployment / metric
	Source  string // pod/xxx、pod/xxx container/yyy、deployment/xxx
	Summary string
	Detail  string
	// Timestamp 为证据观测时刻；Links 为源系统（Prometheus/Grafana）链接。
	Timestamp *time.Time
	Links     []EvidenceLink
}

// EvidenceSet 是证据收集器，供规则注册证据并拿回 ID；按 Kind+Source+Summary 去重。
type EvidenceSet struct {
	items []Evidence
	index map[string]string // 去重键 → ID
}

// NewEvidenceSet 创建空证据集。
func NewEvidenceSet() *EvidenceSet {
	return &EvidenceSet{index: make(map[string]string)}
}

// Add 注册一条证据；签名与 Phase 1 保持一致，时间戳由 RunRules 统一回填。
func (s *EvidenceSet) Add(kind, source, summary, detail string) string {
	return s.AddWith(Evidence{Kind: kind, Source: source, Summary: summary, Detail: detail})
}

// AddWith 注册带完整元数据（时间戳、链接）的证据；内容相同的证据已存在则返回其 ID。
func (s *EvidenceSet) AddWith(ev Evidence) string {
	key := ev.Kind + "|" + ev.Source + "|" + ev.Summary
	if id, ok := s.index[key]; ok {
		return id
	}
	id := fmt.Sprintf("ev-%d", len(s.items)+1)
	ev.ID = id
	s.index[key] = id
	s.items = append(s.items, ev)
	return id
}

// List 返回按注册顺序排列的全部证据。
func (s *EvidenceSet) List() []Evidence {
	return s.items
}
```

- [ ] **Step 4: 在 RunRules 中回填时间戳并处理内存规则去重**

修改 `internal/diagnose/rules.go` 的 `RunRules` 为：

```go
// RunRules 执行全部规则，返回按严重度排序的发现和被引用的证据集。
func RunRules(snap *Snapshot) ([]Finding, []Evidence) {
	evidence := NewEvidenceSet()
	var findings []Finding

	// 先跑具体规则，再由兜底规则处理尚未被覆盖的异常 Pod。
	for _, rule := range rules {
		findings = append(findings, rule(snap, evidence)...)
	}
	covered := make(map[string]bool)
	for _, f := range findings {
		if strings.HasPrefix(f.Subject, "pod/") {
			covered[subjectPodName(f.Subject)] = true
		}
	}
	findings = append(findings, podNotReadyRule(snap, evidence, covered)...)
	// OOM 已覆盖的容器不再单独报内存压力，避免同一现象重复成条。
	findings = removeMemoryPressureUnderOOM(findings)
	sort.SliceStable(findings, func(i, j int) bool {
		return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
	})
	fillEvidenceTimestamps(snap, evidence)
	return findings, evidence.List()
}

// removeMemoryPressureUnderOOM 删除与 OOM 发现对象相同的内存压力发现。
func removeMemoryPressureUnderOOM(findings []Finding) []Finding {
	oomSubjects := map[string]bool{}
	for _, f := range findings {
		if f.Category == "oom" {
			oomSubjects[f.Subject] = true
		}
	}
	out := findings[:0]
	for _, f := range findings {
		if f.Category == "memory-pressure" && oomSubjects[f.Subject] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// fillEvidenceTimestamps 为全部证据补齐观测时刻：
// 事件证据取事件自身 LastTimestamp，其余缺省为快照采集时刻。
func fillEvidenceTimestamps(snap *Snapshot, set *EvidenceSet) {
	for i := range set.items {
		ev := &set.items[i]
		if ev.Timestamp != nil {
			continue
		}
		t := snap.CollectedAt
		if ev.Kind == "event" {
			if ts, ok := eventTimestamp(snap, ev.Source, ev.Detail); ok {
				t = ts
			}
		}
		ev.Timestamp = &t
	}
}

// eventTimestamp 按证据来源与内容（事件 Message）反查事件时间。
func eventTimestamp(snap *Snapshot, source, detail string) (time.Time, bool) {
	switch {
	case strings.HasPrefix(source, "pod/"):
		podName := strings.TrimPrefix(source, "pod/")
		for _, e := range snap.Events[podName] {
			if e.Message == detail {
				return e.LastTimestamp, true
			}
		}
	case strings.HasPrefix(source, "deployment/"):
		for _, e := range snap.DeploymentEvents {
			if e.Message == detail {
				return e.LastTimestamp, true
			}
		}
	}
	return time.Time{}, false
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/diagnose/`
Expected: PASS（含原有全部规则测试）

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/diagnose/
git commit -m "feat(diagnose): Evidence 增加时间戳与链接，新增 AddWith"
```

---

### Task 5: 内存压力与 CPU 限流规则

**Files:**
- Create: `internal/diagnose/metrics_rules.go`
- Modify: `internal/diagnose/rules.go`（rules 切片注册 + oom 规则追加指标证据）
- Test: `internal/diagnose/metrics_rules_test.go`

**Interfaces:**
- Consumes: Task 1 的 `prometheus.Point/ContainerMetrics`、`prometheus.MemoryWorkingSetPromQL`、`prometheus.GraphURL/GrafanaExploreURL`；Task 4 的 Snapshot/Evidence 新字段、`AddWith`
- Produces:
- 规则类别 `memory-pressure`（严重度中）、`cpu-throttle`（严重度低）

- [ ] **Step 1: 写失败测试**

创建 `internal/diagnose/metrics_rules_test.go`：

```go
package diagnose_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/prometheus"
)

func metricsSnapshot(podName string, limit float64, peak float64, throttle float64) *diagnose.Snapshot {
	now := time.Now()
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  podName,
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name: "app", Ready: true,
		}},
	})
	snap.PrometheusURL = "http://prom:9090"
	snap.MetricsWindow = 30 * time.Minute
	m := &prometheus.ContainerMetrics{
		Pod:       podName,
		Container: "app",
		MemoryWorkingSet: []prometheus.Point{
			{T: now.Add(-time.Minute), V: peak},
		},
		MemoryLimit: &limit,
		CPUThrottledRatio: []prometheus.Point{
			{T: now.Add(-time.Minute), V: throttle},
		},
	}
	snap.Metrics = map[string]map[string]*prometheus.ContainerMetrics{
		podName: {"app": m},
	}
	return snap
}

func TestMemoryTrendRuleThreshold(t *testing.T) {
	// 峰值 85%（含）时报内存压力；84% 不报。
	snap := metricsSnapshot("p85", 100, 85, 0)
	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)
	f := findCategory(findings, "memory-pressure")
	if f == nil {
		t.Fatalf("峰值 85%% 应报内存压力，实际 %v", findings)
	}
	if f.Severity != diagnose.SeverityMedium {
		t.Errorf("内存压力严重度应为中，实际 %s", f.Severity)
	}

	snap = metricsSnapshot("p84", 100, 84, 0)
	findings, _ = diagnose.RunRules(snap)
	if findCategory(findings, "memory-pressure") != nil {
		t.Errorf("峰值 84%% 不应报内存压力: %v", findings)
	}
}

func TestMemoryTrendSuppressedByOOM(t *testing.T) {
	snap := metricsSnapshot("pOOM", 100, 95, 0)
	// 追加 OOMKilled 状态。
	snap.Pods[0].Containers[0].Ready = false
	snap.Pods[0].Containers[0].LastTerminated = &kubernetes.Termination{
		Reason: "OOMKilled", ExitCode: 137,
	}
	findings, _ := diagnose.RunRules(snap)
	if findCategory(findings, "memory-pressure") != nil {
		t.Errorf("已有 OOM 发现时不应重复报内存压力: %v", findings)
	}
}

func TestCPUThrottleRuleThreshold(t *testing.T) {
	// 限流比率均值 25%（含）时报；24% 不报。
	snap := metricsSnapshot("pThr", 100, 0, 0.25)
	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)
	f := findCategory(findings, "cpu-throttle")
	if f == nil {
		t.Fatalf("限流 25%% 应报 CPU 限流，实际 %v", findings)
	}
	if f.Severity != diagnose.SeverityLow {
		t.Errorf("CPU 限流严重度应为低，实际 %s", f.Severity)
	}

	snap = metricsSnapshot("pOk", 100, 0, 0.24)
	findings, _ = diagnose.RunRules(snap)
	if findCategory(findings, "cpu-throttle") != nil {
		t.Errorf("限流 24%% 不应报 CPU 限流: %v", findings)
	}
}

func TestOOMRuleIncludesMetricEvidence(t *testing.T) {
	snap := metricsSnapshot("pOOM2", 100, 95, 0)
	snap.Pods[0].Containers[0].Ready = false
	snap.Pods[0].Containers[0].LastTerminated = &kubernetes.Termination{
		Reason: "OOMKilled", ExitCode: 137,
	}
	_, evidence := diagnose.RunRules(snap)
	found := false
	for _, e := range evidence {
		if e.Kind == "metric" {
			found = true
			if len(e.Links) == 0 {
				t.Errorf("指标证据应携带 graph 链接")
			}
		}
	}
	if !found {
		t.Errorf("OOM 诊断应附带指标证据，实际证据: %+v", evidence)
	}
}

func TestMetricsAbsentBehavesLikePhase1(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name: "pPlain", Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{Name: "app", Ready: true}},
	})
	findings, _ := diagnose.RunRules(snap)
	if len(findings) != 0 {
		t.Errorf("无指标且健康时不应有发现: %v", findings)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/diagnose/`
Expected: FAIL（找不到 memory-pressure / cpu-throttle 发现）

- [ ] **Step 3: 实现指标规则与辅助函数**

创建 `internal/diagnose/metrics_rules.go`：

```go
package diagnose

import (
	"fmt"
	"strings"
	"time"

	"opsagent/internal/prometheus"
)

// --- 内存压力规则 ---

// memoryTrendRule 检查每个容器的内存工作集峰值占比，≥85% 时报内存压力。
func memoryTrendRule(snap *Snapshot, set *EvidenceSet) []Finding {
	if len(snap.Metrics) == 0 {
		return nil
	}
	end := snap.CollectedAt
	start := end.Add(-snap.MetricsWindow)

	var findings []Finding
	for podName, containers := range snap.Metrics {
		for containerName, m := range containers {
			if m == nil || m.MemoryLimit == nil || *m.MemoryLimit <= 0 {
				continue
			}
			peak := peakValue(m.MemoryWorkingSet)
			ratio := peak / *m.MemoryLimit
			m.MemoryPeakRatio = ratio
			if ratio < 0.85 {
				continue
			}

			promQL := prometheus.MemoryWorkingSetPromQL(snap.Target.Namespace, []string{podName})
			summary := fmt.Sprintf("容器 %s 内存工作集峰值占 limit 的 %.0f%%", containerName, ratio*100)
			id := set.AddWith(Evidence{
				Kind:      "metric",
				Source:    containerSource(podName, containerName),
				Summary:   summary,
				Detail:    pointsText(m.MemoryWorkingSet),
				Timestamp: &snap.CollectedAt,
				Links:     metricLinks(snap, promQL, start, end),
			})

			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 内存使用逼近上限（峰值 %.0f%%）", containerName, ratio*100),
				Category:    "memory-pressure",
				Severity:    SeverityMedium,
				Subject:     containerSource(podName, containerName),
				Explanation:   fmt.Sprintf("过去 %s 内存工作集峰值达到 limit 的 %.0f%%，已接近 OOM 临界。", snap.MetricsWindow, ratio*100),
				Suggestion:  "结合趋势判断是缓慢增长（排查内存泄漏、缓存/大对象、堆参数）还是真实业务增长；确认根因后再调整 limit。",
				Risk:        "使用继续增长将触发 OOMKill；未确认根因前不要直接调高 limit。",
				EvidenceIDs: []string{id},
			})
		}
	}
	return findings
}

// --- CPU 限流规则 ---

// cpuThrottleRule 检查 CFS 限流时间片比率均值，≥25% 时报 CPU 限流。
func cpuThrottleRule(snap *Snapshot, set *EvidenceSet) []Finding {
	if len(snap.Metrics) == 0 {
		return nil
	}
	end := snap.CollectedAt
	start := end.Add(-snap.MetricsWindow)

	var findings []Finding
	for podName, containers := range snap.Metrics {
		for containerName, m := range containers {
			if m == nil || len(m.CPUThrottledRatio) == 0 {
				continue
			}
			avg := averageValue(m.CPUThrottledRatio)
			if avg < 0.25 {
				continue
			}

			promQL := prometheus.CPUThrottlePromQL(snap.Target.Namespace, []string{podName})
			summary := fmt.Sprintf("容器 %s CPU 限流时间片平均占比 %.0f%%", containerName, avg*100)
			id := set.AddWith(Evidence{
				Kind:      "metric",
				Source:    containerSource(podName, containerName),
				Summary:   summary,
				Detail:    pointsText(m.CPUThrottledRatio),
				Timestamp: &snap.CollectedAt,
				Links:     metricLinks(snap, promQL, start, end),
			})

			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 出现 CPU 限流（平均 %.0f%%）", containerName, avg*100),
				Category:    "cpu-throttle",
				Severity:    SeverityLow,
				Subject:     containerSource(podName, containerName),
				Explanation:   fmt.Sprintf("过去 %s 内约 %.0f%% 的 CPU 调度时间片被限流，进程申请的 CPU 超过了 limit。", snap.MetricsWindow, avg*100),
				Suggestion:  "确认应用是否存在计算尖峰；若长期接近 limit，适当调大 CPU limit 或优化热点逻辑。",
				Risk:        "持续限流会拉长请求延迟、放大超时；盲目调大 limit 会挤占节点容量。",
				EvidenceIDs: []string{id},
			})
		}
	}
	return findings
}

// --- 辅助函数 ---

// metricFor 返回快照中指定容器的指标（不存在返回 nil）。
func metricFor(snap *Snapshot, podName, containerName string) *prometheus.ContainerMetrics {
	if containers, ok := snap.Metrics[podName]; ok {
		return containers[containerName]
	}
	return nil
}

// metricLinks 构造指标证据的 graph/grafana 链接。
func metricLinks(snap *Snapshot, promQL string, start, end time.Time) []EvidenceLink {
	links := []EvidenceLink{{
		Type: "graph", Label: "Prometheus 图表",
		URL: prometheus.GraphURL(snap.PrometheusURL, promQL, start, end),
	}}
	if g := prometheus.GrafanaExploreURL(snap.GrafanaURL, promQL, start, end); g != "" {
		links = append(links, EvidenceLink{Type: "grafana", Label: "Grafana Explore", URL: g})
	}
	return links
}

// pointsText 把指标点序列渲染成文本，最多保留最后 20 个点。
func pointsText(points []prometheus.Point) string {
	if len(points) > 20 {
		points = points[len(points)-20:]
	}
	lines := make([]string, 0, len(points))
	for _, p := range points {
		lines = append(lines, fmt.Sprintf("%s  %g", p.T.Format("2006-01-02 15:04:05"), p.V))
	}
	return strings.Join(lines, "\n")
}

// peakValue 返回点序列的最大值。
func peakValue(points []prometheus.Point) float64 {
	peak := 0.0
	for _, p := range points {
		if p.V > peak {
			peak = p.V
		}
	}
	return peak
}

// averageValue 返回点序列的算术平均值。
func averageValue(points []prometheus.Point) float64 {
	if len(points) == 0 {
		return 0
	}
	sum := 0.0
	for _, p := range points {
		sum += p.V
	}
	return sum / float64(len(points))
}
```

- [ ] **Step 4: 注册规则并增强 oomKilledRule**

修改 `internal/diagnose/rules.go` 的 rules 切片：

```go
var rules = []Rule{
	crashLoopBackOffRule,
	imagePullBackOffRule,
	oomKilledRule,
	pendingPodRule,
	probeFailureRule,
	deploymentRule,
	memoryTrendRule,
	cpuThrottleRule,
}
```

在 `oomKilledRule` 中，把每段 Finding 构造前的证据收集处（`ids := []string{...}` 加完日志证据之后）追加指标证据。最终该段为：

```go
			summary := fmt.Sprintf("容器 %s 因内存超限被杀（exitCode=137）", c.Name)
			ids := []string{set.Add("status", containerSource(pod.Name, c.Name), summary, detail)}
			if id, ok := addLogEvidence(set, snap, pod.Name, c.Name); ok {
				ids = append(ids, id)
			}
			if m := metricFor(snap, pod.Name, c.Name); m != nil && len(m.MemoryWorkingSet) > 0 {
				end := snap.CollectedAt
				start := end.Add(-snap.MetricsWindow)
				promQL := prometheus.MemoryWorkingSetPromQL(snap.Target.Namespace, []string{pod.Name})
				ids = append(ids, set.AddWith(Evidence{
					Kind:      "metric",
					Source:    containerSource(pod.Name, c.Name),
					Summary:   fmt.Sprintf("容器 %s 内存工作集趋势（%d 个采样点）", c.Name, len(m.MemoryWorkingSet)),
					Detail:    pointsText(m.MemoryWorkingSet),
					Timestamp: &snap.CollectedAt,
					Links:     metricLinks(snap, promQL, start, end),
				}))
			}
```

并在 `rules.go` 顶部 import 块加入 `"opsagent/internal/prometheus"`。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/diagnose/`
Expected: PASS（新老用例全部）

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/diagnose/
git commit -m "feat(diagnose): 内存压力与 CPU 限流规则，OOM 附带指标证据"
```

---

### Task 6: Document 模型、组装与 LLM 引用校验

**Files:**
- Create: `internal/report/document.go`
- Create: `internal/report/builder.go`
- Test: `internal/report/builder_test.go`

**Interfaces:**
- Consumes: `diagnose.{Snapshot,Finding,Evidence,EvidenceLink,Target}`、`llm.Analysis`
- Produces:
  - `type Document struct { Title, TargetDisplay, Namespace string; GeneratedAt time.Time; MetricSource string; Notices []string; Findings []DocFinding; LLM *DocLLM; Overview []DocOverviewRow; Evidence []DocEvidence }`
  - `type DocFinding struct { Title, Severity, Subject, Explanation, Suggestion, Risk string }`
  - `type DocEvidence struct { ID, Kind, Source, Summary, Detail string; Timestamp *time.Time; Links []diagnose.EvidenceLink }`
  - `type DocLLM struct { Model, ContentMD string; InvalidCitations []string }`
  - `type DocOverviewRow struct { Name, Detail string }`
  - `func BuildDocument(snap *diagnose.Snapshot, findings []diagnose.Finding, evidence []diagnose.Evidence, analysis *llm.Analysis, notices []string) *Document`
  - `func ValidateCitations(content string, valid map[string]bool) []string`
  - `func NeutralizeCitations(content string, invalid []string) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/report/builder_test.go`：

```go
package report_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/llm"
	"opsagent/internal/report"
)

func TestValidateCitations(t *testing.T) {
	valid := map[string]bool{"ev-1": true, "ev-2": true}
	if got := report.ValidateCitations("依据 [ev-1] 和 [ev-2]", valid); len(got) != 0 {
		t.Errorf("全部合法时不应有缺失，实际 %v", got)
	}
	got := report.ValidateCitations("引用 [ev-7]，还有 [ev-0]，重复 [ev-7]", valid)
	if len(got) != 2 {
		t.Errorf("应找出 ev-7、ev-0 两个（去重），实际 %v", got)
	}
	if got := report.ValidateCitations("无引用", valid); len(got) != 0 {
		t.Errorf("无引用时应返回空，实际 %v", got)
	}
}

func TestNeutralizeCitations(t *testing.T) {
	got := report.NeutralizeCitations("见 [ev-7]", []string{"ev-7"})
	if !stringsContains(got, "ev-7（无效引用）") {
		t.Errorf("无效引用应被中性化标注，实际 %q", got)
	}
	if stringsContains(got, "ev-7]") && !stringsContains(got, "无效引用") {
		t.Errorf("原始引用不应原样保留: %q", got)
	}
}

func TestBuildDocument(t *testing.T) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	snap := &diagnose.Snapshot{
		Target:        diagnose.Target{Kind: "pod", Name: "p1", Namespace: "default"},
		PrometheusURL: "http://prom:9090",
		CollectedAt:   now,
	}
	findings := []diagnose.Finding{{
		Title: "故障", Severity: diagnose.SeverityCritical, Subject: "pod/p1",
		Explanation: "原因", Suggestion: "建议", Risk: "风险",
		EvidenceIDs: []string{"ev-1"},
	}}
	evTime := now
	evidence := []diagnose.Evidence{{
		ID: "ev-1", Kind: "status", Source: "pod/p1", Summary: "状态", Detail: "细节", Timestamp: &evTime,
	}}
	analysis := &llm.Analysis{Model: "test-model", Content: "根因见 [ev-1]，另有 [ev-9]"}

	doc := report.BuildDocument(snap, findings, evidence, analysis, []string{"一条提示"})
	if doc.TargetDisplay != "pod/p1" || doc.Namespace != "default" {
		t.Errorf("目标信息错误: %q %q", doc.TargetDisplay, doc.Namespace)
	}
	if len(doc.Findings) != 1 || doc.Findings[0].Explanation == "原因" {
		t.Errorf("发现的解释应追加行内证据标记，实际 %+v", doc.Findings)
	}
	if !stringsContains(doc.Findings[0].Explanation, "[ev-1]") {
		t.Errorf("解释中缺少行内 [ev-1]: %q", doc.Findings[0].Explanation)
	}
	if doc.LLM == nil || doc.LLM.Model != "test-model" {
		t.Fatalf("LLM 信息缺失: %+v", doc.LLM)
	}
	if len(doc.LLM.InvalidCitations) != 1 || doc.LLM.InvalidCitations[0] != "ev-9" {
		t.Errorf("应校验出 ev-9 无效，实际 %v", doc.LLM.InvalidCitations)
	}
	if stringsContains(doc.LLM.ContentMD, "ev-9（无效引用）") == false {
		t.Errorf("正文中无效引用应被中性化: %q", doc.LLM.ContentMD)
	}
	if len(doc.Notices) != 1 || doc.Notices[0] != "一条提示" {
		t.Errorf("notice 未保留: %v", doc.Notices)
	}
	if len(doc.Evidence) != 1 || doc.Evidence[0].ID != "ev-1" {
		t.Errorf("证据未保留: %+v", doc.Evidence)
	}
}

func TestBuildDocumentWithoutLLM(t *testing.T) {
	snap := &diagnose.Snapshot{
		Target:      diagnose.Target{Kind: "all", Name: "", Namespace: "default"},
		CollectedAt: time.Now(),
	}
	doc := report.BuildDocument(snap, nil, nil, nil, nil)
	if doc.LLM != nil {
		t.Errorf("未开启 LLM 时应为 nil")
	}
	if doc.TargetDisplay != "全部 Pod" {
		t.Errorf("无目标名时应显示全部 Pod，实际 %q", doc.TargetDisplay)
	}
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/report/`
Expected: FAIL（undefined: report.BuildDocument 等）

- [ ] **Step 3: 实现 Document 模型**

创建 `internal/report/document.go`：

```go
// Package report 定义渲染无关的报告模型 Document，并提供 Markdown/HTML 渲染器。
// Document 同时是诊断历史落库与复现的序列化格式。
package report

import (
	"time"

	"opsagent/internal/diagnose"
)

// Document 是一份完整诊断报告的渲染无关表示。
type Document struct {
	Title         string
	TargetDisplay string // pod/p1、deployment/web、全部 Pod
	Namespace     string
	GeneratedAt   time.Time
	MetricSource  string // Prometheus 地址与来源，如 "discovery http://..."；空表示未用指标
	// Notices 是降级与校验提示，渲染在报告顶部。
	Notices []string
	Findings []DocFinding
	LLM      *DocLLM // nil 表示本次未开启或未成功
	Overview []DocOverviewRow
	Evidence []DocEvidence
}

// DocFinding 是一条发现的视图数据，Explanation 已带行内 [ev-x] 标记。
type DocFinding struct {
	Title       string
	Severity    string
	Subject     string
	Explanation string
	Suggestion  string
	Risk        string
}

// DocLLM 是 LLM 分析的视图数据，InvalidCitations 为校验出的编造编号。
type DocLLM struct {
	Model            string
	ContentMD        string
	InvalidCitations []string
}

// DocOverviewRow 是资源概况中的一行。
type DocOverviewRow struct {
	Name   string
	Detail string
}

// DocEvidence 是一条证据的视图数据。
type DocEvidence struct {
	ID        string
	Kind      string
	Source    string
	Summary   string
	Detail    string
	Timestamp *time.Time
	Links     []diagnose.EvidenceLink
}
```

- [ ] **Step 4: 实现组装与引用校验**

创建 `internal/report/builder.go`：

```go
package report

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/llm"
)

// citationPattern 匹配证据编号 ev-数字。
var citationPattern = regexp.MustCompile(`ev-\d+`)

// ValidateCitations 扫描正文中的证据编号，返回 valid 集合中不存在的编号（去重）。
func ValidateCitations(content string, valid map[string]bool) []string {
	var missing []string
	seen := map[string]bool{}
	for _, id := range citationPattern.FindAllString(content, -1) {
		if !valid[id] && !seen[id] {
			missing = append(missing, id)
			seen[id] = true
		}
	}
	return missing
}

// NeutralizeCitations 把无效编号替换为"编号（无效引用）"，保留模型原文以便审计。
func NeutralizeCitations(content string, invalid []string) string {
	for _, id := range invalid {
		content = strings.ReplaceAll(content, id, id+"（无效引用）")
	}
	return content
}

// BuildDocument 由快照、发现、证据和 LLM 分析组装 Document。
func BuildDocument(snap *diagnose.Snapshot, findings []diagnose.Finding, evidence []diagnose.Evidence, analysis *llm.Analysis, notices []string) *Document {
	targetDisplay := "全部 Pod"
	if snap.Target.Name != "" {
		kind := snap.Target.Kind
		if kind == "" {
			kind = "pod"
		}
		targetDisplay = fmt.Sprintf("%s/%s", kind, snap.Target.Name)
	}

	metricSource := ""
	if snap.PrometheusURL != "" {
		metricSource = snap.PrometheusURL
	}

	doc := &Document{
		Title:         "诊断报告",
		TargetDisplay: targetDisplay,
		Namespace:     snap.Target.Namespace,
		GeneratedAt:   snap.CollectedAt,
		MetricSource:  metricSource,
		Notices:       append([]string{}, notices...),
		Findings:      buildFindings(findings),
		Overview:      buildOverview(snap),
		Evidence:      buildEvidence(evidence),
	}

	if analysis != nil {
		valid := map[string]bool{}
		for _, e := range evidence {
			valid[e.ID] = true
		}
		invalid := ValidateCitations(analysis.Content, valid)
		content := analysis.Content
		if len(invalid) > 0 {
			content = NeutralizeCitations(content, invalid)
			doc.Notices = append(doc.Notices,
				fmt.Sprintf("LLM 引用了不存在的证据编号：%s，已在正文中标注", strings.Join(invalid, "、")))
		}
		doc.LLM = &DocLLM{Model: analysis.Model, ContentMD: content, InvalidCitations: invalid}
	}
	return doc
}

// buildFindings 把 Finding 转成视图结构，在解释末尾追加行内证据标记。
func buildFindings(findings []diagnose.Finding) []DocFinding {
	out := make([]DocFinding, 0, len(findings))
	for _, f := range findings {
		markers := make([]string, len(f.EvidenceIDs))
		for i, id := range f.EvidenceIDs {
			markers[i] = "[" + id + "]"
		}
		explanation := f.Explanation
		if len(markers) > 0 {
			explanation = explanation + "（证据: " + strings.Join(markers, "、") + "）"
		}
		out = append(out, DocFinding{
			Title:       f.Title,
			Severity:    string(f.Severity),
			Subject:     f.Subject,
			Explanation: explanation,
			Suggestion:  f.Suggestion,
			Risk:        f.Risk,
		})
	}
	return out
}

// buildOverview 生成 Deployment 与各 Pod 的概况行。
func buildOverview(snap *diagnose.Snapshot) []DocOverviewRow {
	var out []DocOverviewRow
	if d := snap.Deployment; d != nil {
		out = append(out, DocOverviewRow{
			Name: "Deployment/" + d.Name,
			Detail: fmt.Sprintf("desired=%d updated=%d ready=%d available=%d",
				d.Replicas.Desired, d.Replicas.Updated, d.Replicas.Ready, d.Replicas.Available),
		})
	}
	for _, pod := range snap.Pods {
		ready := 0
		for _, c := range pod.Containers {
			if c.Ready {
				ready++
			}
		}
		out = append(out, DocOverviewRow{
			Name:   "Pod/" + pod.Name,
			Detail: fmt.Sprintf("phase=%s ready=%d/%d", pod.Phase, ready, len(pod.Containers)),
		})
	}
	return out
}

// buildEvidence 把证据转成视图结构。
func buildEvidence(evidence []diagnose.Evidence) []DocEvidence {
	out := make([]DocEvidence, 0, len(evidence))
	for _, e := range evidence {
		var ts *time.Time
		if e.Timestamp != nil {
			t := *e.Timestamp
			ts = &t
		}
		out = append(out, DocEvidence{
			ID:        e.ID,
			Kind:      e.Kind,
			Source:    e.Source,
			Summary:   e.Summary,
			Detail:    e.Detail,
			Timestamp: ts,
			Links:     append([]diagnose.EvidenceLink{}, e.Links...),
		})
	}
	return out
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/report/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/report/
go test ./...
git commit -m "feat(report): Document 模型、组装与 LLM 引用校验"
```

---

### Task 7: 渲染器接口与 Markdown 渲染器迁移

**Files:**
- Modify: `internal/report/markdown.go`（整体重写为消费 Document）
- Create: `internal/report/renderer.go`
- Test: `internal/report/markdown_test.go`

**Interfaces:**
- Consumes: Task 6 的 Document/DocFinding/DocEvidence
- Produces:
  - `type Renderer interface { Render(w io.Writer, doc *Document) error }`
  - `func For(format string) (Renderer, error)`
  - `func FormatFromPath(path string) string`
  - `func RenderMarkdown(doc *Document) string`
  - 格式常量字符串：`"markdown"`、`"html"`

- [ ] **Step 1: 写失败测试**

创建 `internal/report/markdown_test.go`：

```go
package report_test

import (
	"strings"
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/report"
)

func sampleDoc() *report.Document {
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	return &report.Document{
		Title:         "诊断报告",
		TargetDisplay: "pod/p1",
		Namespace:     "default",
		GeneratedAt:   ts,
		MetricSource:  "http://prom:9090",
		Notices:       []string{"一条提示"},
		Findings: []report.DocFinding{{
			Title:       "故障标题",
			Severity:    "严重",
			Subject:     "pod/p1",
			Explanation: "故障解释（证据: [ev-1]、[ev-2]）",
			Suggestion:  "处理建议",
			Risk:        "风险提示",
		}},
		Evidence: []report.DocEvidence{{
			ID: "ev-1", Kind: "metric", Source: "pod/p1", Summary: "指标摘要",
			Detail: "10:00  0.9", Timestamp: &ts,
			Links: []diagnose.EvidenceLink{
				{Type: "graph", Label: "Prometheus 图表", URL: "http://prom/graph?x=1"},
				{Type: "grafana", Label: "Grafana Explore", URL: "http://grafana/explore?x=1"},
			},
		}},
	}
}

func TestRenderMarkdown(t *testing.T) {
	got := report.RenderMarkdown(sampleDoc())
	wantSubstrings := []string{
		"# 诊断报告",
		"pod/p1",
		"## 提示",
		"一条提示",
		"## 结论摘要",
		"故障标题",
		"## 发现详情",
		"故障解释（证据: [ev-1]、[ev-2]）",
		"处理建议",
		"风险提示",
		"## 证据链",
		"指标摘要",
		"http://prom/graph?x=1",
		"http://grafana/explore?x=1",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("Markdown 缺少 %q", sub)
		}
	}
}

func TestRenderMarkdownNoFindings(t *testing.T) {
	doc := sampleDoc()
	doc.Findings = nil
	doc.Evidence = nil
	doc.Notices = nil
	got := report.RenderMarkdown(doc)
	if !strings.Contains(got, "未发现确定性异常") {
		t.Errorf("无发现时应给出结论，实际:\n%s", got)
	}
}

func TestRendererForAndFormatFromPath(t *testing.T) {
	md, err := report.For("markdown")
	if err != nil {
		t.Fatalf("markdown 渲染器: %v", err)
	}
	var sb strings.Builder
	if err := md.Render(&sb, sampleDoc()); err != nil || sb.Len() == 0 {
		t.Errorf("Renderer.Render 失败: %v len=%d", err, sb.Len())
	}
	if _, err := report.For("pdf"); err == nil {
		t.Errorf("不支持的格式应返回错误")
	}
	if got := report.FormatFromPath("/tmp/a.MD"); got != "markdown" {
		t.Errorf("扩展名识别错误: %q", got)
	}
	if got := report.FormatFromPath("/tmp/b.html"); got != "html" {
		t.Errorf("扩展名识别错误: %q", got)
	}
	if got := report.FormatFromPath("/tmp/c.txt"); got != "" {
		t.Errorf("未知扩展名应返回空串: %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/report/`
Expected: FAIL（undefined: RenderMarkdown(doc)/For/FormatFromPath；旧 RenderMarkdown 签名不符导致编译失败）

- [ ] **Step 3: 实现渲染器接口**

创建 `internal/report/renderer.go`：

```go
package report

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// 支持的格式名。
const (
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

// Renderer 把 Document 渲染到 w。
type Renderer interface {
	Render(w io.Writer, doc *Document) error
}

// For 按格式名返回渲染器。
func For(format string) (Renderer, error) {
	switch format {
	case FormatMarkdown:
		return MarkdownRenderer{}, nil
	case FormatHTML:
		return HTMLRenderer{}, nil
	default:
		return nil, fmt.Errorf("不支持的格式 %q，支持: markdown、html", format)
	}
}

// FormatFromPath 按文件扩展名推断格式；.md→markdown，.html/.htm→html；其余返回空串。
func FormatFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		return FormatMarkdown
	case ".html", ".htm":
		return FormatHTML
	default:
		return ""
	}
}

// MarkdownRenderer 实现 Markdown 渲染。
type MarkdownRenderer struct{}

// Render 实现 Renderer。
func (MarkdownRenderer) Render(w io.Writer, doc *Document) error {
	_, err := io.WriteString(w, RenderMarkdown(doc))
	return err
}
```

- [ ] **Step 4: 迁移 Markdown 渲染器**

整体重写 `internal/report/markdown.go`：

```go
package report

import (
	"fmt"
	"strings"
)

// RenderMarkdown 把 Document 渲染成 Markdown 报告。
func RenderMarkdown(doc *Document) string {
	var b strings.Builder

	b.WriteString("# " + doc.Title + "\n\n")
	b.WriteString(fmt.Sprintf("- 诊断对象: %s\n", doc.TargetDisplay))
	b.WriteString(fmt.Sprintf("- 命名空间: %s\n", doc.Namespace))
	b.WriteString(fmt.Sprintf("- 采集时间: %s\n", doc.GeneratedAt.Format("2006-01-02 15:04:05")))
	if doc.MetricSource != "" {
		b.WriteString(fmt.Sprintf("- 指标来源: %s\n", doc.MetricSource))
	}
	b.WriteString("\n")

	if len(doc.Notices) > 0 {
		b.WriteString("## 提示\n\n")
		for _, n := range doc.Notices {
			b.WriteString("> " + n + "\n>\n")
		}
		b.WriteString("\n")
	}

	if len(doc.Findings) == 0 {
		b.WriteString("## 结论\n\n未发现确定性异常。\n\n")
		writeLLMSection(&b, doc.LLM)
		writeOverviewSection(&b, doc.Overview)
		writeEvidenceSection(&b, doc.Evidence)
		return b.String()
	}

	b.WriteString("## 结论摘要\n\n")
	for i, f := range doc.Findings {
		b.WriteString(fmt.Sprintf("%d. **[%s]** %s（%s）\n", i+1, f.Severity, f.Title, f.Subject))
	}
	b.WriteString("\n")

	b.WriteString("## 发现详情\n\n")
	for i, f := range doc.Findings {
		b.WriteString(fmt.Sprintf("### %d. %s\n\n", i+1, f.Title))
		b.WriteString(fmt.Sprintf("- 严重程度: %s\n", f.Severity))
		b.WriteString(fmt.Sprintf("- 对象: %s\n", f.Subject))
		b.WriteString(fmt.Sprintf("- 分析: %s\n", f.Explanation))
		b.WriteString(fmt.Sprintf("- 建议: %s\n", f.Suggestion))
		b.WriteString(fmt.Sprintf("- 风险提示: %s\n\n", f.Risk))
	}

	writeLLMSection(&b, doc.LLM)
	writeOverviewSection(&b, doc.Overview)
	writeEvidenceSection(&b, doc.Evidence)
	return b.String()
}

// writeLLMSection 渲染 LLM 章节；nil 时跳过。
func writeLLMSection(b *strings.Builder, llm *DocLLM) {
	if llm == nil {
		return
	}
	b.WriteString("## LLM 根因分析\n\n")
	b.WriteString(fmt.Sprintf("> 由模型 %s 基于证据生成，仅供参考；请结合证据链自行判断。\n\n", llm.Model))
	b.WriteString(strings.TrimSpace(llm.ContentMD))
	b.WriteString("\n\n")
}

// writeOverviewSection 渲染资源概况。
func writeOverviewSection(b *strings.Builder, rows []DocOverviewRow) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("## 资源概况\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("- %s: %s\n", r.Name, r.Detail))
	}
	b.WriteString("\n")
}

// writeEvidenceSection 按类型分组渲染证据，附带时间戳与链接。
func writeEvidenceSection(b *strings.Builder, evidence []DocEvidence) {
	if len(evidence) == 0 {
		return
	}
	b.WriteString("## 证据链\n\n")
	groups := []struct {
		kind, title string
	}{
		{"deployment", "Deployment 状态"},
		{"status", "资源状态"},
		{"metric", "指标"},
		{"event", "事件"},
		{"log", "日志"},
	}
	for _, g := range groups {
		var items []DocEvidence
		for _, e := range evidence {
			if e.Kind == g.kind {
				items = append(items, e)
			}
		}
		if len(items) == 0 {
			continue
		}
		b.WriteString("### " + g.title + "\n\n")
		for _, e := range items {
			header := fmt.Sprintf("**[%s] %s — %s**", e.ID, e.Source, e.Summary)
			if e.Timestamp != nil {
				header += fmt.Sprintf("（%s）", e.Timestamp.Format("2006-01-02 15:04:05"))
			}
			b.WriteString(header + "\n\n")
			if strings.TrimSpace(e.Detail) != "" {
				b.WriteString("```\n")
				b.WriteString(strings.TrimRight(e.Detail, "\n"))
				b.WriteString("\n```\n\n")
			}
			for _, link := range e.Links {
				b.WriteString(fmt.Sprintf("- [%s](%s)\n", link.Label, link.URL))
			}
			if len(e.Links) > 0 {
				b.WriteString("\n")
			}
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/report/`
Expected: PASS

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS（cli 中调用旧 RenderMarkdown 的代码此时编译失败——先执行 Task 13 会破坏顺序。因此本任务暂时不提交 cli 引用：确认当前 cli/diagnose.go 的调用为 `report.RenderMarkdown(snap, findings, evidence, analysis)`，签名已变。为保持中间状态可编译，本步骤在 cli/diagnose.go 中临时改为不调用渲染器，改为占位。）

在 `internal/cli/diagnose.go` 中，把 markdown 分支临时替换为：

```go
		case "markdown":
			return fmt.Errorf("Markdown 渲染迁移中，请在后续任务完成后使用")
```

Run: `go test ./...`
Expected: PASS

```bash
git add internal/report/ internal/cli/diagnose.go
git commit -m "feat(report): 渲染器接口与 Markdown 渲染器迁移"
```

---

### Task 8: HTML 渲染器

**Files:**
- Create: `internal/report/html.go`
- Create: `internal/report/report.html`
- Test: `internal/report/html_test.go`

**Interfaces:**
- Consumes: Task 6 的 Document；Task 7 的 Renderer/For
- Produces:
  - `type HTMLRenderer struct{}`
  - `func RenderHTML(doc *Document) string`
  - `HTMLRenderer` 实现 `Renderer`（Task 7 的 For("html") 已引用）

- [ ] **Step 1: 写失败测试**

创建 `internal/report/html_test.go`：

```go
package report_test

import (
	"strings"
	"testing"

	"opsagent/internal/report"
)

func TestRenderHTML(t *testing.T) {
	doc := sampleDoc()
	got := report.RenderHTML(doc)

	checks := []string{
		"<!doctype html>",
		"诊断报告",
		"pod/p1",
		"一条提示",
		"故障标题",
		`href="#ev-1"`,
		`id="ev-1"`,
		"<details",
		"http://prom/graph?x=1",
		`target="_blank"`,
	}
	for _, sub := range checks {
		if !strings.Contains(got, sub) {
			t.Errorf("HTML 缺少 %q", sub)
		}
	}
	// 特殊字符必须被转义，不能出现可执行标签。
	doc.Evidence[0].Detail = "<script>alert(1)</script>"
	got = report.RenderHTML(doc)
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Errorf("证据 Detail 未转义: %s", got)
	}
}

func TestRenderHTMLWithLLM(t *testing.T) {
	doc := sampleDoc()
	doc.LLM = &report.DocLLM{
		Model:     "test-model",
		ContentMD: "## 根因\n\n- 内存泄漏，见 [ev-1]",
	}
	got := report.RenderHTML(doc)
	if !strings.Contains(got, "根因") || !strings.Contains(got, "内存泄漏") {
		t.Errorf("LLM Markdown 正文未渲染: %s", got)
	}
	if !strings.Contains(got, "test-model") {
		t.Errorf("缺少模型名: %s", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/report/`
Expected: FAIL（undefined: RenderHTML；HTMLRenderer 在 For 中被引用但未实现会导致编译失败）

- [ ] **Step 3: 创建 HTML 模板**

创建 `internal/report/report.html`：

```html
<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>{{.Title}} - {{.TargetDisplay}}</title>
<style>
  body { font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; margin: 2rem auto; max-width: 960px; color: #1f2937; line-height: 1.6; }
  h1 { border-bottom: 2px solid #e5e7eb; padding-bottom: .4rem; }
  h2 { margin-top: 2rem; color: #111827; }
  .notice { background: #fef3c7; border-left: 4px solid #f59e0b; padding: .5rem 1rem; margin: .5rem 0; border-radius: 2px; }
  .finding { border: 1px solid #e5e7eb; border-radius: 6px; padding: 1rem; margin: 1rem 0; }
  .finding .sev-critical { color: #b91c1c; font-weight: 700; }
  .finding .sev-high { color: #c2410c; font-weight: 700; }
  details.evidence { border: 1px solid #e5e7eb; border-radius: 4px; margin: .5rem 0; padding: .5rem 1rem; }
  details.evidence summary { cursor: pointer; font-weight: 600; }
  pre { background: #f9fafb; border-radius: 4px; padding: .8rem; overflow-x: auto; font-size: .85rem; }
  a { color: #2563eb; }
  .meta { color: #6b7280; font-size: .9rem; }
  .llm { background: #eff6ff; border-left: 4px solid #3b82f6; padding: .5rem 1rem; border-radius: 2px; }
</style>
</head>
<body>
<h1>{{.Title}}</h1>
<p class="meta">
  诊断对象: {{.TargetDisplay}} ｜ 命名空间: {{.Namespace}} ｜
  采集时间: {{timestamp .GeneratedAt}}
  {{if .MetricSource}} ｜ 指标来源: {{.MetricSource}}{{end}}
</p>

{{if .Notices}}
<h2>提示</h2>
{{range .Notices}}<div class="notice">{{.}}</div>{{end}}
{{end}}

{{if .Findings}}
<h2>结论摘要</h2>
<ol>
{{range .Findings}}
  <li><span class="sev-{{severityClass .Severity}}">[{{.Severity}}]</span> {{.Title}}（{{.Subject}}）</li>
{{end}}
</ol>

<h2>发现详情</h2>
{{range .Findings}}
<div class="finding">
  <h3>{{.Title}}</h3>
  <p><strong>严重程度:</strong> <span class="sev-{{severityClass .Severity}}">{{.Severity}}</span></p>
  <p><strong>对象:</strong> {{.Subject}}</p>
  <p><strong>分析:</strong> {{linkify .Explanation}}</p>
  <p><strong>建议:</strong> {{.Suggestion}}</p>
  <p><strong>风险提示:</strong> {{.Risk}}</p>
</div>
{{end}}
{{else}}
<h2>结论</h2>
<p>未发现确定性异常。</p>
{{end}}

{{with .LLM}}
<h2>LLM 根因分析</h2>
<div class="llm">
<p class="meta">由模型 {{.Model}} 基于证据生成，仅供参考；请结合证据链自行判断。</p>
{{markdown .ContentMD}}
</div>
{{end}}

{{if .Overview}}
<h2>资源概况</h2>
<ul>
{{range .Overview}}<li>{{.Name}}: {{.Detail}}</li>{{end}}
</ul>
{{end}}

{{if .Evidence}}
<h2>证据链</h2>
{{range .Evidence}}
<details class="evidence">
  <summary id="{{.ID}}">[{{.ID}}] {{.Source}} — {{.Summary}}{{if .Timestamp}}（{{timestamp .Timestamp}}）{{end}}</summary>
  {{if .Detail}}<pre>{{.Detail}}</pre>{{end}}
  {{range .Links}}<p><a href="{{.URL}}" target="_blank" rel="noopener">{{.Label}}</a></p>{{end}}
</details>
{{end}}
{{end}}
</body>
</html>
```

- [ ] **Step 4: 实现 HTML 渲染器**

创建 `internal/report/html.go`：

```go
package report

import (
	"bytes"
	_ "embed"
	"html"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

//go:embed report.html
var reportHTMLTemplate string

// evMarkerPattern 匹配行内证据标记 [ev-数字]。
var evMarkerPattern = regexp.MustCompile(`\[(ev-\d+)\]`)

// HTMLRenderer 实现 HTML 渲染。
type HTMLRenderer struct{}

// Render 实现 Renderer。
func (HTMLRenderer) Render(w io.Writer, doc *Document) error {
	_, err := io.WriteString(w, RenderHTML(doc))
	return err
}

// RenderHTML 把 Document 渲染成独立 HTML（内嵌样式，无外部资源）。
func RenderHTML(doc *Document) string {
	tmpl := template.Must(template.New("report").Funcs(template.FuncMap{
		"timestamp": func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"severityClass": func(severity string) string {
			switch severity {
			case "严重":
				return "critical"
			case "高":
				return "high"
			default:
				return "normal"
			}
		},
		"linkify":   linkifyCitations,
		"markdown":  renderMarkdownToHTML,
	}).Parse(reportHTMLTemplate))

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, doc); err != nil {
		return "<p>HTML 渲染失败: " + html.EscapeString(err.Error()) + "</p>"
	}
	return buf.String()
}

// linkifyCitations 先转义纯文本，再把 [ev-x] 转成页内锚点链接。
// 只用于 Finding 等结构化安全字段，证据 Detail 不经过此函数。
func linkifyCitations(text string) template.HTML {
	escaped := html.EscapeString(text)
	linked := evMarkerPattern.ReplaceAllString(escaped, `<a href="#$1">[$1]</a>`)
	return template.HTML(linked)
}

// renderMarkdownToHTML 把 LLM 的 Markdown 正文转为 HTML；goldmark 默认不透传 raw HTML。
func renderMarkdownToHTML(content string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(strings.TrimSpace(content)), &buf); err != nil {
		return template.HTML("<pre>" + html.EscapeString(content) + "</pre>")
	}
	return template.HTML(buf.String())
}
```

补上 html.go 缺失的 import（`io`），import 块应为：

```go
import (
	"bytes"
	_ "embed"
	"html"
	"html/template"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)
```

- [ ] **Step 5: 获取依赖并运行测试**

Run: `go get github.com/yuin/goldmark && go mod tidy`
Expected: 依赖添加成功

Run: `go test ./internal/report/`
Expected: PASS

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/report/ go.mod go.sum
git commit -m "feat(report): 独立 HTML 渲染器（折叠证据、锚点、Markdown 正文）"
```

---

### Task 9: SQLite 打开与内嵌迁移

**Files:**
- Create: `internal/storage/sqlite.go`
- Create: `internal/storage/migrate.go`
- Test: `internal/storage/sqlite_test.go`

**Interfaces:**
- Produces:
  - `type DB struct`（内含 `*sql.DB`，包内可见）
  - `func Open(path string) (*DB, error)`
  - `func DefaultPath() (string, error)`
  - `func (db *DB) Close() error`

- [ ] **Step 1: 写失败测试**

创建 `internal/storage/sqlite_test.go`：

```go
package storage_test

import (
	"path/filepath"
	"strings"
	"testing"

	"opsagent/internal/storage"
)

func TestOpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// user_version 应为 1。
	var version int
	if err := db.QueryRowForTest(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("查询 user_version 失败: %v", err)
	}
	if version != 1 {
		t.Errorf("迁移后 user_version 应为 1，实际 %d", version)
	}

	// diagnosis_records 表必须存在。
	var name string
	err = db.QueryRowForTest(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='diagnosis_records'`).Scan(&name)
	if err != nil || name != "diagnosis_records" {
		t.Errorf("diagnosis_records 表未创建: %v", err)
	}
}

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db1, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := storage.Open(path) // 再次打开不应重复迁移报错
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	db2.Close()
}

func TestDefaultPath(t *testing.T) {
	got, err := storage.DefaultPath()
	if err != nil {
		t.Fatalf("默认路径: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(got), "opsagent/history.db") {
		t.Errorf("默认路径应以 opsagent/history.db 结尾，实际 %q", got)
	}
}
```

`QueryRowForTest` 不是真实 API。改用导出的测试辅助不可行，因此让 `DB` 暴露一个仅供测试的方法。在本任务 Step 3 的 sqlite.go 中加入：

```go
// QueryRowForTest 暴露一行查询供包外测试使用。
func (db *DB) QueryRowForTest(query string) *sql.Row {
	return db.sqlDB.QueryRow(query)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/storage/`
Expected: FAIL（undefined: storage.Open）

- [ ] **Step 3: 实现打开逻辑**

创建 `internal/storage/sqlite.go`：

```go
// Package storage 用纯 Go SQLite 保存诊断历史。
// 本包不依赖 diagnose 等领域包：记录负载为 report.Document 的 JSON 字节。
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/glebarez/go-sqlite"
)

// DB 包装 SQLite 连接。
type DB struct {
	sqlDB *sql.DB
}

// Open 打开（不存在则创建）数据库，创建父目录并执行迁移。
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db := &DB{sqlDB: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// DefaultPath 返回默认数据库路径：os.UserConfigDir()/opsagent/history.db。
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户配置目录: %w", err)
	}
	return filepath.Join(dir, "opsagent", "history.db"), nil
}

// Close 关闭数据库连接。
func (db *DB) Close() error { return db.sqlDB.Close() }

// QueryRowForTest 暴露一行查询，仅供测试。
func (db *DB) QueryRowForTest(query string) *sql.Row {
	return db.sqlDB.QueryRow(query)
}
```

- [ ] **Step 4: 实现迁移**

创建 `internal/storage/migrate.go`：

```go
package storage

import (
	"database/sql"
	"fmt"
)

// schemaV1 是第一个版本的表结构。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS diagnosis_records (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at       TEXT    NOT NULL,
  namespace        TEXT    NOT NULL,
  target_kind      TEXT    NOT NULL,
  target_name      TEXT    NOT NULL DEFAULT '',
  target_display   TEXT    NOT NULL,
  findings_count   INTEGER NOT NULL DEFAULT 0,
  highest_severity TEXT    NOT NULL DEFAULT '',
  duration_ms      INTEGER NOT NULL DEFAULT 0,
  prometheus_url   TEXT    NOT NULL DEFAULT '',
  llm_model        TEXT    NOT NULL DEFAULT '',
  schema_version   INTEGER NOT NULL,
  payload          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_diagnosis_created_at ON diagnosis_records (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_diagnosis_ns_time    ON diagnosis_records (namespace, created_at DESC);
`

// migrations 按版本号排列 DDL。
var migrations = map[int]string{
	1: schemaV1,
}

// migrate 读取 PRAGMA user_version，在事务内逐版本应用 DDL。
func (db *DB) migrate() error {
	var version int
	if err := db.sqlDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}
	for v := version + 1; v <= len(migrations); v++ {
		if err := applyVersion(db.sqlDB, v, migrations[v]); err != nil {
			return err
		}
	}
	return nil
}

// applyVersion 在一个事务内执行 DDL 并更新 user_version。
func applyVersion(sqlDB *sql.DB, version int, ddl string) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("开始迁移事务失败: %w", err)
	}
	if _, err := tx.Exec(ddl); err != nil {
		tx.Rollback()
		return fmt.Errorf("应用 v%d DDL 失败: %w", version, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
		tx.Rollback()
		return fmt.Errorf("更新数据库版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: 获取依赖并运行测试**

Run: `go get github.com/glebarez/go-sqlite && go mod tidy`
Expected: 依赖添加成功

Run: `go test ./internal/storage/`
Expected: PASS

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/storage/ go.mod go.sum
git commit -m "feat(storage): 纯 Go SQLite 打开与 user_version 迁移"
```

---

### Task 10: 诊断记录仓储

**Files:**
- Create: `internal/storage/repository.go`
- Create: `internal/storage/sqlite_repository.go`
- Test: `internal/storage/repository_test.go`

**Interfaces:**
- Produces:
  - `type Record struct { ID int64; CreatedAt time.Time; Namespace, TargetKind, TargetName, TargetDisplay string; FindingsCount int; HighestSeverity string; DurationMS int64; PrometheusURL, LLMModel string; SchemaVersion int; Payload []byte }`
  - `type RecordSummary struct`（Record 去掉 Payload）
  - `var ErrNotFound = errors.New(...)`
  - `type Repository interface { Save(ctx, Record)(int64,error); List(ctx, namespace string, limit int)([]RecordSummary,error); Get(ctx, id int64)(*Record,error); Latest(ctx)(*Record,error) }`
  - `*DB` 实现 Repository

- [ ] **Step 1: 写失败测试**

创建 `internal/storage/repository_test.go`：

```go
package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"opsagent/internal/storage"
)

func sampleRecord(createdAt time.Time, namespace string) storage.Record {
	return storage.Record{
		CreatedAt:       createdAt,
		Namespace:       namespace,
		TargetKind:      "pod",
		TargetName:      "p1",
		TargetDisplay:   "pod/p1",
		FindingsCount:   2,
		HighestSeverity: "严重",
		DurationMS:      123,
		PrometheusURL:   "http://prom:9090",
		LLMModel:        "test-model",
		SchemaVersion:   1,
		Payload:         []byte(`{"title":"诊断报告"}`),
	}
}

func openTestDB(t *testing.T) storage.Repository {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSaveAndGet(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	created := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	id, err := repo.Save(ctx, sampleRecord(created, "default"))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if id != 1 {
		t.Errorf("首条记录 id 应为 1，实际 %d", id)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Namespace != "default" || got.TargetDisplay != "pod/p1" || got.FindingsCount != 2 {
		t.Errorf("标量字段往返错误: %+v", got)
	}
	if string(got.Payload) != `{"title":"诊断报告"}` {
		t.Errorf("payload 往返错误: %s", got.Payload)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("时间往返错误: %v", got.CreatedAt)
	}
}

func TestListSummaryAndFilter(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	if _, err := repo.Save(ctx, sampleRecord(time.Now().Add(-2*time.Hour), "default")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Save(ctx, sampleRecord(time.Now().Add(-time.Hour), "production")); err != nil {
		t.Fatal(err)
	}

	all, err := repo.List(ctx, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应有 2 条摘要，实际 %d", len(all))
	}
	// 按 created_at 倒序：production 记录更新，应排第一。
	if all[0].Namespace != "production" {
		t.Errorf("排序应为时间倒序，实际首条 %q", all[0].Namespace)
	}
	for _, s := range all {
		if len(s.Payload) != 0 {
			t.Errorf("摘要不应包含 payload")
		}
	}

	prod, err := repo.List(ctx, "production", 50)
	if err != nil || len(prod) != 1 || prod[0].Namespace != "production" {
		t.Errorf("namespace 过滤错误: %v %d", err, len(prod))
	}
}

func TestLatest(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	if _, err := repo.Latest(ctx); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("空库 Latest 应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := repo.Save(ctx, sampleRecord(time.Now(), "default")); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Latest(ctx)
	if err != nil {
		t.Fatalf("非空库 Latest 失败: %v", err)
	}
	if got.TargetDisplay != "pod/p1" {
		t.Errorf("Latest 内容错误: %+v", got)
	}
}

func TestGetUnknownID(t *testing.T) {
	repo := openTestDB(t)
	_, err := repo.Get(context.Background(), 999)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("未知 ID 应返回 ErrNotFound，实际 %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/storage/`
Expected: FAIL（undefined: Repository / Record / ErrNotFound）

- [ ] **Step 3: 定义仓储接口与记录类型**

创建 `internal/storage/repository.go`：

```go
package storage

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示按 id 查询或空库取最近记录时无数据。
var ErrNotFound = errors.New("诊断记录不存在")

// Record 是一次诊断的完整持久化记录。
type Record struct {
	ID              int64
	CreatedAt       time.Time
	Namespace       string
	TargetKind      string
	TargetName      string
	TargetDisplay   string
	FindingsCount   int
	HighestSeverity string
	DurationMS      int64
	PrometheusURL   string
	LLMModel        string
	SchemaVersion   int
	Payload         []byte
}

// RecordSummary 是列表用的摘要，不含 payload。
type RecordSummary struct {
	ID              int64
	CreatedAt       time.Time
	Namespace       string
	TargetKind      string
	TargetName      string
	TargetDisplay   string
	FindingsCount   int
	HighestSeverity string
	DurationMS      int64
	PrometheusURL   string
	LLMModel        string
}

// Repository 是诊断历史的持久化边界。
type Repository interface {
	Save(ctx context.Context, r Record) (int64, error)
	List(ctx context.Context, namespace string, limit int) ([]RecordSummary, error)
	Get(ctx context.Context, id int64) (*Record, error)
	Latest(ctx context.Context) (*Record, error)
}
```

- [ ] **Step 4: 实现 SQLite 仓储**

创建 `internal/storage/sqlite_repository.go`：

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 确保 *DB 实现 Repository。
var _ Repository = (*DB)(nil)

// Save 插入一条记录，返回自增 id。时间统一以 RFC3339 UTC 存储。
func (db *DB) Save(ctx context.Context, r Record) (int64, error) {
	createdAt := r.CreatedAt.UTC().Format(time.RFC3339)
	res, err := db.sqlDB.ExecContext(ctx, `
INSERT INTO diagnosis_records
  (created_at, namespace, target_kind, target_name, target_display,
   findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
   schema_version, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		createdAt, r.Namespace, r.TargetKind, r.TargetName, r.TargetDisplay,
		r.FindingsCount, r.HighestSeverity, r.DurationMS, r.PrometheusURL, r.LLMModel,
		r.SchemaVersion, string(r.Payload))
	if err != nil {
		return 0, fmt.Errorf("保存诊断记录失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取记录 id 失败: %w", err)
	}
	return id, nil
}

// List 返回摘要列表，可按命名空间过滤，按 created_at 倒序；limit<=0 时默认 50。
func (db *DB) List(ctx context.Context, namespace string, limit int) ([]RecordSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model
FROM diagnosis_records`
	args := []interface{}{}
	if namespace != "" {
		query += ` WHERE namespace = ?`
		args = append(args, namespace)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询诊断历史失败: %w", err)
	}
	defer rows.Close()

	var out []RecordSummary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get 按 id 取完整记录。
func (db *DB) Get(ctx context.Context, id int64) (*Record, error) {
	row := db.sqlDB.QueryRowContext(ctx, `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
       schema_version, payload
FROM diagnosis_records WHERE id = ?`, id)
	return scanRecord(row)
}

// Latest 取最近一条完整记录。
func (db *DB) Latest(ctx context.Context) (*Record, error) {
	row := db.sqlDB.QueryRowContext(ctx, `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
       schema_version, payload
FROM diagnosis_records ORDER BY created_at DESC LIMIT 1`)
	return scanRecord(row)
}

// rowScanner 同时兼容 sql.Row 与 sql.Rows。
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// scanSummary 扫描一行摘要。
func scanSummary(s rowScanner) (RecordSummary, error) {
	var (
		summary     RecordSummary
		createdAt   string
		promURL     string
		llmModel    string
	)
	err := s.Scan(&summary.ID, &createdAt, &summary.Namespace, &summary.TargetKind,
		&summary.TargetName, &summary.TargetDisplay, &summary.FindingsCount,
		&summary.HighestSeverity, &summary.DurationMS, &promURL, &llmModel)
	if err != nil {
		return RecordSummary{}, err
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return RecordSummary{}, fmt.Errorf("解析记录时间失败: %w", err)
	}
	summary.CreatedAt = t
	summary.PrometheusURL = promURL
	summary.LLMModel = llmModel
	return summary, nil
}

// scanRecord 扫描一条完整记录，无行时返回 ErrNotFound。
func scanRecord(s rowScanner) (*Record, error) {
	var (
		rec       Record
		createdAt string
		payload   string
	)
	err := s.Scan(&rec.ID, &createdAt, &rec.Namespace, &rec.TargetKind,
		&rec.TargetName, &rec.TargetDisplay, &rec.FindingsCount,
		&rec.HighestSeverity, &rec.DurationMS, &rec.PrometheusURL, &rec.LLMModel,
		&rec.SchemaVersion, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取诊断记录失败: %w", err)
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("解析记录时间失败: %w", err)
	}
	rec.CreatedAt = t
	rec.Payload = []byte(payload)
	return &rec, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/storage/`
Expected: PASS

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/storage/
git commit -m "feat(storage): 诊断记录 Save/List/Get/Latest 仓储"
```

---

### Task 11: OpenTelemetry 自身追踪初始化

**Files:**
- Create: `internal/observability/tracing.go`
- Test: `internal/observability/tracing_test.go`

**Interfaces:**
- Produces:
  - `const SpanDiagnose, SpanCollect, SpanPrometheus, SpanRules, SpanLLM, SpanBuild, SpanPersist string`
  - `func Init(ctx context.Context) (shutdown func(context.Context) error, err error)`
  - `func Tracer() trace.Tracer`

- [ ] **Step 1: 写失败测试**

创建 `internal/observability/tracing_test.go`：

```go
package observability_test

import (
	"context"
	"testing"

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

	ctx, cancel := context.WithTimeout(context.Background(), 1e9)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown 失败: %v", err)
	}
}
```

`context.WithTimeout(ctx, 1e9)` 的 1e9 是纳秒 = 1s，写法可工作但不直观；改为 `time.Second`，import 加入 `"time"`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/observability/`
Expected: FAIL（undefined: observability.Init）

- [ ] **Step 3: 实现追踪初始化**

创建 `internal/observability/tracing.go`：

```go
// Package observability 负责 opsagent 自身的可观测性：OpenTelemetry trace 初始化。
// 未配置 OTLP endpoint 时完全 noop：不启动 goroutine、不发起连接。
package observability

import (
	"context"
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
```

补上 import：`"errors"` 不需要；需要 `"fmt"`。

- [ ] **Step 4: 获取依赖并运行测试**

Run: `go mod tidy`
Expected: OpenTelemetry 依赖添加成功

Run: `go test ./internal/observability/`
Expected: PASS

- [ ] **Step 5: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/observability/ go.mod go.sum
git commit -m "feat(observability): OpenTelemetry trace 初始化，未配置即 noop"
```

---

### Task 12: Agent 固定阶段管线

**Files:**
- Create: `internal/agent/runner.go`
- Test: `internal/agent/runner_test.go`

**Interfaces:**
- Consumes:
  - `diagnose.Collect(ctx, k8sclient.Interface, diagnose.Target, time.Duration, int64) (*diagnose.Snapshot, error)`
  - `diagnose.RunRules(*diagnose.Snapshot) ([]diagnose.Finding, []diagnose.Evidence)`
  - `prometheus.Querier`
  - `storage.Repository`、`storage.Record`
  - `report.BuildDocument`、`report.Document`
  - `observability.Tracer()`、span 常量
  - `llm.LoadConfig()`、`llm.Analyze()`
- Produces:
  - `type Deps struct { K8s k8sclient.Interface; Prom prometheus.Querier; PromURL string; Repo storage.Repository }`
  - `type Input struct { Target diagnose.Target; Since time.Duration; Tail int64; UseLLM bool; ModelOverride string; MetricsWindow time.Duration; GrafanaURL string; Notices []string }`
  - `type Output struct { Document *report.Document; Snapshot *diagnose.Snapshot; RecordID int64; Notices []string }`
  - `func New(deps Deps) *Runner`
  - `func (r *Runner) Run(ctx context.Context, in Input) (*Output, error)`

纪律：agent 包只允许固定阶段，不引入 Plan/Step/工具注册表。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/runner_test.go`：

```go
package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"opsagent/internal/agent"
	"opsagent/internal/diagnose"
	"opsagent/internal/prometheus"
	"opsagent/internal/storage"
)

// fakeQuerier 是可切换报错的指标查询假实现。
type fakeQuerier struct {
	err error
}

func (f *fakeQuerier) Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*prometheus.ContainerMetrics, error) {
	return nil, f.err
}

// fakeRepo 是可切换保存报错的仓储假实现。
type fakeRepo struct {
	saveErr error
}

func (f *fakeRepo) Save(ctx context.Context, r storage.Record) (int64, error) {
	if f.saveErr != nil {
		return 0, f.saveErr
	}
	return 42, nil
}
func (f *fakeRepo) List(ctx context.Context, namespace string, limit int) ([]storage.RecordSummary, error) {
	return nil, nil
}
func (f *fakeRepo) Get(ctx context.Context, id int64) (*storage.Record, error) {
	return nil, storage.ErrNotFound
}
func (f *fakeRepo) Latest(ctx context.Context) (*storage.Record, error) {
	return nil, storage.ErrNotFound
}

// healthyReadyPod 构造一个运行正常、不会触发日志采集的 Pod。
func healthyReadyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-"+name)},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:v1"}},
		},
	}
}

func runWith(t *testing.T, deps agent.Deps) *agent.Output {
	t.Helper()
	r := agent.New(deps)
	out, err := r.Run(context.Background(), agent.Input{
		Target:        diagnose.Target{Kind: "pod", Name: "p1", Namespace: "default"},
		Since:         30 * time.Minute,
		Tail:          100,
		MetricsWindow: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	return out
}

func TestRunHappyPath(t *testing.T) {
	var k8s kubernetes.Interface = fake.NewSimpleClientset(healthyReadyPod("p1"))
	out := runWith(t, agent.Deps{K8s: k8s})
	if out.Document == nil {
		t.Fatalf("应产出 Document")
	}
	if out.RecordID != 0 {
		t.Errorf("未提供 Repo 时不应落库，实际 %d", out.RecordID)
	}
}

func TestRunMetricsFailureProducesNotice(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	deps := agent.Deps{
		K8s:     k8s,
		Prom:     &fakeQuerier{err: errors.New("prometheus 超时")},
		PromURL:  "http://prom:9090",
	}
	out := runWith(t, deps)
	if out.Document == nil {
		t.Fatalf("指标失败不应阻断报告")
	}
	found := false
	for _, n := range out.Notices {
		if stringsContains(n, "prometheus 超时") {
			found = true
		}
	}
	if !found {
		t.Errorf("应在 Notices 中包含指标降级原因，实际 %v", out.Notices)
	}
}

func TestRunPersistFailureProducesNotice(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	deps := agent.Deps{
		K8s:  k8s,
		Repo: &fakeRepo{saveErr: errors.New("disk full")},
	}
	out := runWith(t, deps)
	if out.Document == nil {
		t.Fatalf("落库失败不应阻断 Document")
	}
	if out.RecordID != 0 {
		t.Errorf("保存失败时 RecordID 应为 0，实际 %d", out.RecordID)
	}
	found := false
	for _, n := range out.Notices {
		if stringsContains(n, "disk full") {
			found = true
		}
	}
	if !found {
		t.Errorf("应在 Notices 中包含落库失败原因，实际 %v", out.Notices)
	}
}

func TestRunPersistSuccessSetsRecordID(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	out := runWith(t, agent.Deps{K8s: k8s, Repo: &fakeRepo{}})
	if out.RecordID != 42 {
		t.Errorf("落库成功应返回记录 id，实际 %d", out.RecordID)
	}
}

func TestRunCollectFailureHardFails(t *testing.T) {
	// 空 fake clientset，Get 不存在的 Pod 应硬失败。
	r := agent.New(agent.Deps{K8s: fake.NewSimpleClientset()})
	_, err := r.Run(context.Background(), agent.Input{
		Target: diagnose.Target{Kind: "pod", Name: "missing", Namespace: "default"},
	})
	if err == nil {
		t.Errorf("采集失败应返回错误")
	}
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/agent/`
Expected: FAIL（undefined: agent.New）

- [ ] **Step 3: 实现 Runner**

创建 `internal/agent/runner.go`：

```go
// Package agent 实现固定阶段的诊断管线，是 CLI/TUI/MCP 复用的统一入口。
// 纪律：只允许 Runner 一个固定管线，不引入 Plan/Step/工具注册表。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	k8sclient "k8s.io/client-go/kubernetes"

	"opsagent/internal/diagnose"
	"opsagent/internal/llm"
	"opsagent/internal/observability"
	"opsagent/internal/prometheus"
	"opsagent/internal/report"
	"opsagent/internal/storage"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Deps 是管线的外部依赖，全部显式注入；Prom/Repo 可为 nil。
type Deps struct {
	K8s     k8sclient.Interface
	Prom    prometheus.Querier
	PromURL string
	Repo    storage.Repository
}

// Input 是一次诊断请求的参数。
type Input struct {
	Target        diagnose.Target
	Since         time.Duration
	Tail          int64
	UseLLM        bool
	ModelOverride string
	MetricsWindow time.Duration
	GrafanaURL    string
	Notices       []string // 装配阶段已产生的降级提示
}

// Output 是管线产出：Document 供渲染，Snapshot 供表格视图。
type Output struct {
	Document *report.Document
	Snapshot *diagnose.Snapshot
	RecordID int64
	Notices  []string
}

// Runner 执行固定阶段诊断管线。
type Runner struct {
	deps Deps
}

// New 创建 Runner。
func New(deps Deps) *Runner {
	return &Runner{deps: deps}
}

// Run 依次执行 collect→metrics→rules→llm→build→persist。
func (r *Runner) Run(ctx context.Context, in Input) (*Output, error) {
	ctx, rootSpan := observability.Tracer().Start(ctx, observability.SpanDiagnose)
	defer rootSpan.End()

	startedAt := time.Now()
	notices := append([]string{}, in.Notices...)
	addNotice := func(format string, args ...interface{}) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}
	tracer := observability.Tracer()

	// 1) collect：硬失败。
	var snap *diagnose.Snapshot
	if err := stage(ctx, tracer, observability.SpanCollect, func(ctx context.Context) error {
		var err error
		snap, err = diagnose.Collect(ctx, r.deps.K8s, in.Target, in.Since, in.Tail)
		return err
	}); err != nil {
		rootSpan.RecordError(err)
		return nil, err
	}

	// 2) metrics：失败降级。
	if err := stage(ctx, tracer, observability.SpanPrometheus, func(ctx context.Context) error {
		snap.PrometheusURL = r.deps.PromURL
		snap.MetricsWindow = in.MetricsWindow
		snap.GrafanaURL = in.GrafanaURL
		if r.deps.Prom == nil {
			return fmt.Errorf("Prometheus 未配置或不可达，跳过指标分析")
		}
		podNames := make([]string, 0, len(snap.Pods))
		for _, p := range snap.Pods {
			podNames = append(podNames, p.Name)
		}
		metrics, err := r.deps.Prom.Query(ctx, snap.Target.Namespace, podNames, in.MetricsWindow)
		if err != nil {
			return err
		}
		snap.Metrics = metrics
		if len(metrics) == 0 {
			return fmt.Errorf("Prometheus 返回空指标")
		}
		return nil
	}); err != nil {
		addNotice("指标阶段：%v", err)
	}

	// 3) rules。
	var findings []diagnose.Finding
	var evidence []diagnose.Evidence
	stage(ctx, tracer, observability.SpanRules, func(ctx context.Context) error {
		findings, evidence = diagnose.RunRules(snap)
		return nil
	})

	// 4) llm：失败降级。
	var analysis *llm.Analysis
	if in.UseLLM {
		if err := stage(ctx, tracer, observability.SpanLLM, func(ctx context.Context) error {
			cfg, err := llm.LoadConfig()
			if err != nil {
				return err
			}
			if in.ModelOverride != "" {
				cfg.Model = in.ModelOverride
			}
			analysis, err = llm.Analyze(ctx, cfg, snap, findings, evidence)
			return err
		}); err != nil {
			addNotice("LLM 分析失败：%v，本次仅输出规则分析", err)
			analysis = nil
		}
	}

	// 5) build Document。
	var doc *report.Document
	stage(ctx, tracer, observability.SpanBuild, func(ctx context.Context) error {
		doc = report.BuildDocument(snap, findings, evidence, analysis, notices)
		return nil
	})

	// 6) persist：失败降级。
	var recordID int64
	if r.deps.Repo != nil {
		if err := stage(ctx, tracer, observability.SpanPersist, func(ctx context.Context) error {
			id, err := r.saveRecord(ctx, doc, snap, startedAt, analysis)
			if err != nil {
				return err
			}
			recordID = id
			return nil
		}); err != nil {
			// Document 已构建，需把降级提示同时追加进 doc.Notices，保证渲染可见。
			msg := fmt.Sprintf("历史记录保存失败：%v", err)
			notices = append(notices, msg)
			doc.Notices = append(doc.Notices, msg)
			recordID = 0
		}
	}

	return &Output{
		Document: doc,
		Snapshot: snap,
		RecordID: recordID,
		Notices:  notices,
	}, nil
}

// saveRecord 把 Document 序列化为 payload 并写入仓储。
func (r *Runner) saveRecord(ctx context.Context, doc *report.Document, snap *diagnose.Snapshot, startedAt time.Time, analysis *llm.Analysis) (int64, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}

	llmModel := ""
	if analysis != nil {
		llmModel = analysis.Model
	}
	targetKind := snap.Target.Kind
	if targetKind == "" {
		targetKind = "all"
	}

	return r.deps.Repo.Save(ctx, storage.Record{
		CreatedAt:       doc.GeneratedAt,
		Namespace:       doc.Namespace,
		TargetKind:      targetKind,
		TargetName:      snap.Target.Name,
		TargetDisplay:   doc.TargetDisplay,
		FindingsCount:   len(doc.Findings),
		HighestSeverity: highestSeverity(doc.Findings),
		DurationMS:      time.Since(startedAt).Milliseconds(),
		PrometheusURL:   snap.PrometheusURL,
		LLMModel:        llmModel,
		SchemaVersion:   1,
		Payload:         payload,
	})
}

// highestSeverity 在未排序的发现中取最高严重度；无发现时返回空串。
func highestSeverity(findings []report.DocFinding) string {
	highest := ""
	best := 99
	for _, f := range findings {
		if rank := docSeverityRank(f.Severity); rank < best {
			best = rank
			highest = f.Severity
		}
	}
	return highest
}

// docSeverityRank 与 diagnose 规则的严重度顺序一致，未知严重度排最后。
func docSeverityRank(severity string) int {
	switch severity {
	case "严重":
		return 0
	case "高":
		return 1
	case "中":
		return 2
	case "低":
		return 3
	default:
		return 4
	}
}

// stage 执行一个命名阶段并打 span；返回阶段错误供调用方决定降级。
func stage(ctx context.Context, tracer trace.Tracer, name string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, name)
	defer span.End()
	if err := fn(ctx); err != nil {
		span.SetAttributes(attribute.Bool("opsagent.ok", false))
		span.AddEvent("降级: " + err.Error())
		return err
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/agent/`
Expected: PASS

- [ ] **Step 5: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/agent/
git commit -m "feat(agent): 固定阶段诊断管线，外部依赖统一降级"
```

---

### Task 13: CLI diagnose 改造（新 flags、--last、装配与导出）

**Files:**
- Modify: `internal/cli/root.go`
- Modify: `internal/cli/diagnose.go`
- Test: `internal/cli/diagnose_flags_test.go`

**Interfaces:**
- Consumes:
  - `agent.New(agent.Deps)` / `Runner.Run(ctx, agent.Input)`
  - `prometheus.ResolveURL(ctx, explicit, k8s)`、`prometheus.New(prometheus.Config)`
  - `storage.Open(path)`、`storage.DefaultPath()`、`storage.Repository`
  - `report.For(format)`、`report.FormatFromPath(path)`
  - `kubernetes.NewClient`
- Produces: diagnose 命令 flags：`--prometheus-url`、`--grafana-url`、`--metrics-window`、`--export`、`--format`、`--last`；root 持久 flag：`--history-db`

- [ ] **Step 1: 写失败测试**

创建 `internal/cli/diagnose_flags_test.go`：

```go
package cli

import (
	"path/filepath"
	"testing"
)

func TestExportFormat(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		explicit string
		want     string
		wantErr  bool
	}{
		{"扩展名 md", "a.md", "", "markdown", false},
		{"扩展名 html", "a.html", "", "html", false},
		{"format 优先", "a.md", "html", "html", false},
		{"未知扩展名", "a.txt", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exportFormat(tc.path, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Errorf("期望报错")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("exportFormat = %q, %v，期望 %q", got, err, tc.want)
			}
		})
	}
}

func TestLastMutuallyExclusive(t *testing.T) {
	if err := validateLastArgs(true, []string{"pod/p1"}); err == nil {
		t.Errorf("--last 与诊断目标同时给出应报错")
	}
	if err := validateLastArgs(false, []string{"pod/p1"}); err != nil {
		t.Errorf("正常诊断不应报错: %v", err)
	}
	if err := validateLastArgs(true, nil); err != nil {
		t.Errorf("单独 --last 不应报错: %v", err)
	}
}

func TestLastEmptyHistory(t *testing.T) {
	// 空库 --last 应返回明确错误（用临时 DB 路径）。
	rootCmd.SetArgs([]string{"diagnose", "--last", "--history-db", emptyDBPath(t)})
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	if err == nil {
		t.Errorf("空库 --last 应返回错误")
	}
}

func emptyDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "history.db")
}
```

注意：`rootCmd.Execute()` 会消费全局 flags 并输出错误；`TestLastEmptyHistory` 对空目录调用 Open 时表为空，runLast 返回「暂无诊断历史」错误。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cli/`
Expected: FAIL（undefined: exportFormat / validateLastArgs）

- [ ] **Step 3: root.go 增加 context 入参与 --history-db**

修改 `internal/cli/root.go`：

```go
// Package cli 定义 opsagent 的命令行入口，所有子命令都挂在根命令上。
package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// rootCmd 是整个 CLI 的根命令。
var rootCmd = &cobra.Command{
	Use:   "opsagent",
	Short: "Kubernetes 运维诊断工具",
	Long:  "opsagent 是一个只读的 Kubernetes 故障诊断工具，采集集群证据并生成分析报告。",
}

// Execute 把外部 context 注入根命令并执行。
func Execute(ctx context.Context) error {
	rootCmd.SetContext(ctx)
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringP("namespace", "n", "default", "Kubernetes 命名空间")
	rootCmd.PersistentFlags().String("kubeconfig", "", "kubeconfig 文件路径（默认用 KUBECONFIG 环境变量或 ~/.kube/config）")
	rootCmd.PersistentFlags().String("history-db", "", "SQLite 历史数据库路径（默认用户配置目录下 opsagent/history.db，或用 OPSAGENT_HISTORY_DB）")
}
```

import 加入 `"context"`。

- [ ] **Step 4: 重写 diagnose.go**

整体重写 `internal/cli/diagnose.go`：

```go
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"opsagent/internal/agent"
	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/observability"
	"opsagent/internal/prometheus"
	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// diagnoseCmd 对应 `opsagent diagnose`，只负责参数解析、依赖装配与最终渲染输出。
var diagnoseCmd = &cobra.Command{
	Use:   "diagnose [资源类型/资源名 | 资源类型 资源名]",
	Short: "诊断指定的 Kubernetes 资源",
	Long:  "采集指定资源（pod/deployment）的状态、事件、日志、指标等证据，由规则引擎输出带证据链的诊断报告，并保存为历史记录。",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, _ := cmd.Flags().GetString("namespace")
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		since, _ := cmd.Flags().GetDuration("since")
		tail, _ := cmd.Flags().GetInt64("tail")
		output, _ := cmd.Flags().GetString("output")
		useLLM, _ := cmd.Flags().GetBool("llm")
		modelOverride, _ := cmd.Flags().GetString("llm-model")
		promURLFlag, _ := cmd.Flags().GetString("prometheus-url")
		grafanaURL, _ := cmd.Flags().GetString("grafana-url")
		if grafanaURL == "" {
			grafanaURL = os.Getenv("OPSAGENT_GRAFANA_URL")
		}
		metricsWindow, _ := cmd.Flags().GetDuration("metrics-window")
		exportPath, _ := cmd.Flags().GetString("export")
		formatFlag, _ := cmd.Flags().GetString("format")
		last, _ := cmd.Flags().GetBool("last")

		if err := validateLastArgs(last, args); err != nil {
			return err
		}
		if last {
			return runLast(cmd, namespace, output, exportPath, formatFlag)
		}

		// 导出格式在采集前确定，避免采完才发现格式不支持。
		var exportFormatName string
		if exportPath != "" {
			f, err := exportFormat(exportPath, formatFlag)
			if err != nil {
				return err
			}
			exportFormatName = f
		}
		if output == "table" && exportPath != "" {
			return fmt.Errorf("表格格式不支持导出，请改用 markdown 或 html")
		}

		client, err := kubernetes.NewClient(kubeconfig)
		if err != nil {
			return err
		}

		// Prometheus 地址解析；不可达时 Prom 置 nil，管线降级为纯规则。
		var promQuerier prometheus.Querier
		var resolvedURL string
		var preNotices []string
		baseURL, _, err := prometheus.ResolveURL(cmd.Context(), promURLFlag, client)
		if err == nil {
			querier, qerr := prometheus.New(prometheus.Config{BaseURL: baseURL})
			if qerr != nil {
				preNotices = append(preNotices, fmt.Sprintf("Prometheus 客户端创建失败：%v，跳过指标分析", qerr))
			} else {
				promQuerier = querier
				resolvedURL = baseURL
			}
		}

		// 历史库 best-effort：打不开只警告，不影响诊断。
		var repo storage.Repository
		dbPath, _ := cmd.Flags().GetString("history-db")
		if dbPath == "" {
			dbPath = os.Getenv("OPSAGENT_HISTORY_DB")
		}
		if dbPath == "" {
			dbPath, _ = storage.DefaultPath()
		}
		if db, oerr := storage.Open(dbPath); oerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "警告: 历史数据库不可用（%v），本次不保存历史\n\n", oerr)
		} else {
			repo = db
		}

		target := parseTarget(args)
		target.Namespace = namespace

		r := agent.New(agent.Deps{
			K8s:     client,
			Prom:     promQuerier,
			PromURL:  resolvedURL,
			Repo:     repo,
		})
		out, err := r.Run(cmd.Context(), agent.Input{
			Target:        target,
			Since:         since,
			Tail:          tail,
			UseLLM:        useLLM,
			ModelOverride: modelOverride,
			MetricsWindow: metricsWindow,
			GrafanaURL:    grafanaURL,
			Notices:       preNotices,
		})
		if err != nil {
			return err
		}

		// 降级提示同时输出到 stderr（报告正文 Notices 区也会展示）。
		for _, n := range out.Document.Notices {
			fmt.Fprintf(cmd.ErrOrStderr(), "提示: %s\n", n)
		}

		if exportPath != "" {
			if err := writeExport(exportPath, exportFormatName, out.Document); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "已导出报告到 %s（历史记录 #%d）\n", exportPath, out.RecordID)
			return nil
		}

		switch output {
		case "markdown":
			renderer, err := report.For("markdown")
			if err != nil {
				return err
			}
			return renderer.Render(cmd.OutOrStdout(), out.Document)
		case "table":
			renderTable(cmd.OutOrStdout(), out.Snapshot)
			return nil
		default:
			return fmt.Errorf("未知输出格式 %q，支持: markdown、table", output)
		}
	},
}

// validateLastArgs 校验 --last 不能与诊断目标同时给出。
func validateLastArgs(last bool, args []string) error {
	if last && len(args) > 0 {
		return fmt.Errorf("--last 用于查看最近一次诊断，不能同时指定诊断目标")
	}
	return nil
}

// exportFormat 确定导出格式：explicit 优先，否则按扩展名推断；无法推断时报错。
func exportFormat(path, explicit string) (string, error) {
	if explicit != "" {
		switch explicit {
		case "markdown", "md", "html":
			if explicit == "md" {
				return "markdown", nil
			}
			return explicit, nil
		default:
			return "", fmt.Errorf("不支持的格式 %q，支持: markdown、html", explicit)
		}
	}
	if f := report.FormatFromPath(path); f != "" {
		return f, nil
	}
	return "", fmt.Errorf("无法从文件名 %q 推断格式，请用 --format 指定（支持: markdown、html）", path)
}

// writeExport 用指定格式渲染 Document 并写入文件。
func writeExport(path, format string, doc *report.Document) error {
	renderer, err := report.For(format)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建导出文件 %s 失败: %w", path, err)
	}
	defer f.Close()
	if err := renderer.Render(f, doc); err != nil {
		return fmt.Errorf("写入导出文件失败: %w", err)
	}
	return nil
}

// runLast 处理 --last：不触碰集群，取最近记录渲染（可选导出）。
func runLast(cmd *cobra.Command, namespace, output, exportPath, formatFlag string) error {
	ctx, span := observability.Tracer().Start(cmd.Context(), "history.latest")
	defer span.End()

	dbPath, _ := cmd.Flags().GetString("history-db")
	if dbPath == "" {
		dbPath = os.Getenv("OPSAGENT_HISTORY_DB")
	}
	if dbPath == "" {
		dbPath, _ = storage.DefaultPath()
	}
	repo, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("打开历史数据库失败: %w", err)
	}
	defer repo.Close()
	rec, err := repo.Latest(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("暂无诊断历史")
		}
		return err
	}

	var doc report.Document
	if uerr := json.Unmarshal(rec.Payload, &doc); uerr != nil {
		return fmt.Errorf("解析历史记录失败: %w", uerr)
	}

	if exportPath != "" {
		f, ferr := exportFormat(exportPath, formatFlag)
		if ferr != nil {
			return ferr
		}
		if werr := writeExport(exportPath, f, &doc); werr != nil {
			return werr
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "已导出报告到 %s（历史记录 #%d）\n", exportPath, rec.ID)
		return nil
	}

	f := "markdown"
	if formatFlag != "" {
		if formatFlag == "md" {
			f = "markdown"
		} else {
			f = formatFlag
		}
	}
	if output == "table" {
		return fmt.Errorf("--last 不支持表格格式")
	}
	renderer, err := report.For(f)
	if err != nil {
		return err
	}
	return renderer.Render(cmd.OutOrStdout(), &doc)
}

func init() {
	diagnoseCmd.Flags().Duration("since", 30*time.Minute, "日志采集的时间窗口")
	diagnoseCmd.Flags().Int64("tail", 100, "每个容器最多取的日志行数")
	diagnoseCmd.Flags().StringP("output", "o", "markdown", "输出格式: markdown、table")
	diagnoseCmd.Flags().Bool("llm", false, "启用 LLM 根因分析（需配置 OPSAGENT_LLM_* 环境变量）")
	diagnoseCmd.Flags().String("llm-model", "", "覆盖 OPSAGENT_LLM_MODEL 指定的模型")
	diagnoseCmd.Flags().String("prometheus-url", "", "Prometheus 地址（默认取 OPSAGENT_PROMETHEUS_URL 或集群内自动探测）")
	diagnoseCmd.Flags().String("grafana-url", "", "Grafana 地址，用于生成 Explore 链接（或用 OPSAGENT_GRAFANA_URL）")
	diagnoseCmd.Flags().Duration("metrics-window", 30*time.Minute, "Prometheus 指标查询窗口")
	diagnoseCmd.Flags().String("export", "", "把报告导出到指定文件（格式按扩展名或 --format）")
	diagnoseCmd.Flags().String("format", "", "强制指定输出/导出格式: markdown、html")
	diagnoseCmd.Flags().Bool("last", false, "查看最近一次诊断（不采集集群）")
}

// renderTable 以表格形式输出 Pod 摘要，并附上各 Pod 的 Warning 事件。
func renderTable(out io.Writer, snap *diagnose.Snapshot) {
	if d := snap.Deployment; d != nil {
		fmt.Fprintf(out, "Deployment: %s (desired=%d ready=%d available=%d)\n\n",
			d.Name, d.Replicas.Desired, d.Replicas.Ready, d.Replicas.Available)
	}

	if len(snap.Pods) == 0 {
		fmt.Fprintf(out, "命名空间 %q 中没有 Pod\n", snap.Target.Namespace)
		return
	}

	fmt.Fprintf(out, "命名空间: %s\n\n", snap.Target.Namespace)

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "POD\tPHASE\tREADY\tRESTARTS\tSTATUS")
	for _, pod := range snap.Pods {
		ready := 0
		restarts := int32(0)
		reasons := make([]string, 0)
		for _, c := range pod.Containers {
			if c.Ready {
				ready++
			}
			restarts += c.Restarts
			if c.Reason != "" {
				reasons = append(reasons, fmt.Sprintf("%s: %s", c.Name, c.Reason))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d\t%s\n",
			pod.Name, pod.Phase, ready, len(pod.Containers), restarts, strings.Join(reasons, ", "))
	}
	w.Flush()

	for _, pod := range snap.Pods {
		if len(snap.Events[pod.Name]) > 0 {
			fmt.Fprintf(out, "\n%s 的 Warning 事件:\n", pod.Name)
		}
		for _, e := range snap.Events[pod.Name] {
			fmt.Fprintf(out, "  - [%s] %s (x%d)\n", e.Reason, e.Message, e.Count)
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/cli/`
Expected: PASS

注意：`TestLastEmptyHistory` 中 rootCmd 被 SetArgs 后，其它测试若共享状态可能受影响；测试结束已 SetArgs(nil) 复位。

- [ ] **Step 6: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/cli/
git commit -m "feat(cli): diagnose 接入管线，支持指标、导出与 --last"
```

---

### Task 14: history 命令（list/show/export）

**Files:**
- Create: `internal/cli/history.go`
- Test: `internal/cli/history_test.go`

**Interfaces:**
- Consumes: `storage.Open`、`storage.Repository`、`report.For`、`report.Document`、`exportFormat`（Task 13）、`writeExport`（Task 13）
- Produces: `opsagent history list|show|export`

- [ ] **Step 1: 写失败测试**

创建 `internal/cli/history_test.go`：

```go
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// jsonMarshalDoc 序列化 Document 作为历史 payload。
func jsonMarshalDoc(doc *report.Document) ([]byte, error) { return json.Marshal(doc) }

// bytesContains 判断 s 是否包含 sub。
func bytesContains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// osReadFile 读取整个文件。
func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// idToString 把记录 id 转成命令行使用的字符串。
func idToString(id int64) string { return strconv.FormatInt(id, 10) }

// seedHistory 在临时 DB 中写入一条带 Document payload 的记录。
func seedHistory(t *testing.T, ns string) (dbPath string, id int64) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "history.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doc := &report.Document{
		Title: "诊断报告", TargetDisplay: "pod/p1", Namespace: ns, GeneratedAt: time.Now(),
		Findings: []report.DocFinding{{Title: "测试故障", Severity: "严重", Explanation: "分析"}},
	}
	payload, err := jsonMarshalDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	id, err = db.Save(t.Context(), storage.Record{
		CreatedAt: time.Now(), Namespace: ns, TargetKind: "pod", TargetName: "p1",
		TargetDisplay: "pod/p1", FindingsCount: 1, HighestSeverity: "严重",
		SchemaVersion: 1, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dbPath, id
}

func executeWithHistoryDB(args []string, dbPath string) (stdout, stderr bytes.Buffer, err error) {
	rootCmd.SetArgs(append(args, "--history-db", dbPath))
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	err = rootCmd.Execute()
	rootCmd.SetArgs(nil)
	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)
	return stdout, stderr, err
}

func TestHistoryList(t *testing.T) {
	dbPath, _ := seedHistory(t, "default")
	stdout, _, err := executeWithHistoryDB([]string{"history", "list"}, dbPath)
	if err != nil {
		t.Fatalf("history list 失败: %v", err)
	}
	got := stdout.String()
	if !bytesContains(got, "ID") || !bytesContains(got, "pod/p1") {
		t.Errorf("history list 输出缺少表头或目标: %q", got)
	}
}

func TestHistoryShow(t *testing.T) {
	dbPath, id := seedHistory(t, "default")
	stdout, _, err := executeWithHistoryDB([]string{"history", "show", idToString(id)}, dbPath)
	if err != nil {
		t.Fatalf("history show 失败: %v", err)
	}
	got := stdout.String()
	if !bytesContains(got, "# 诊断报告") || !bytesContains(got, "测试故障") {
		t.Errorf("history show 未复现报告: %q", got)
	}
}

func TestHistoryExport(t *testing.T) {
	dbPath, id := seedHistory(t, "default")
	exportFile := filepath.Join(t.TempDir(), "report.html")
	_, _, err := executeWithHistoryDB(
		[]string{"history", "export", idToString(id), "-o", exportFile}, dbPath)
	if err != nil {
		t.Fatalf("history export 失败: %v", err)
	}
	data, err := osReadFile(exportFile)
	if err != nil {
		t.Fatalf("读取导出文件失败: %v", err)
	}
	if !bytesContains(string(data), "<!doctype html>") {
		t.Errorf("导出内容不是 HTML: %q", string(data)[:200])
	}
}

func TestHistoryShowUnknownID(t *testing.T) {
	dbPath, _ := seedHistory(t, "default")
	_, _, err := executeWithHistoryDB([]string{"history", "show", "999"}, dbPath)
	if err == nil {
		t.Errorf("show 不存在的 id 应报错")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cli/`
Expected: FAIL（unknown command "history"）

- [ ] **Step 3: 实现 history 命令**

创建 `internal/cli/history.go`：

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"opsagent/internal/observability"
	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// historyCmd 聚合历史记录子命令。
var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "查看和导出诊断历史",
}

// historyListCmd 以表格列出历史摘要。
var historyListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出诊断历史",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.list")
		defer span.End()

		repo, closeFn, err := openRepo(cmd)
		if err != nil {
			return err
		}
		defer closeFn()

		namespace, _ := cmd.Flags().GetString("namespace")
		summaries, err := repo.List(ctx, namespace, 50)
		if err != nil {
			return err
		}
		renderHistoryList(cmd.OutOrStdout(), summaries)
		return nil
	},
}

// historyShowCmd 完整复现一条历史报告。
var historyShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "显示指定诊断报告",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.show")
		defer span.End()

		doc, closeFn, err := loadHistoryDocument(ctx, cmd, args[0])
		if err != nil {
			return err
		}
		defer closeFn()

		format, _ := cmd.Flags().GetString("format")
		f := normalizeFormat(format)
		renderer, err := report.For(f)
		if err != nil {
			return err
		}
		return renderer.Render(cmd.OutOrStdout(), doc)
	},
}

// historyExportCmd 把一条历史报告导出到文件。
var historyExportCmd = &cobra.Command{
	Use:   "export <id>",
	Short: "导出指定诊断报告",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.export")
		defer span.End()

		doc, closeFn, err := loadHistoryDocument(ctx, cmd, args[0])
		if err != nil {
			return err
		}
		defer closeFn()

		output, _ := cmd.Flags().GetString("output")
		if output == "" {
			return fmt.Errorf("请用 -o 指定导出文件")
		}
		format, _ := cmd.Flags().GetString("format")
		f, err := exportFormat(output, format)
		if err != nil {
			return err
		}
		return writeExport(output, f, doc)
	},
}

// openRepo 按 --history-db / OPSAGENT_HISTORY_DB / 默认路径打开仓储。
func openRepo(cmd *cobra.Command) (storage.Repository, func(), error) {
	path, _ := cmd.Flags().GetString("history-db")
	if path == "" {
		path = os.Getenv("OPSAGENT_HISTORY_DB")
	}
	if path == "" {
		path, _ = storage.DefaultPath()
	}
	db, err := storage.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开历史数据库失败: %w", err)
	}
	return db, func() { db.Close() }, nil
}

// loadHistoryDocument 读取一条记录并把 payload 反序列化为 Document。
func loadHistoryDocument(ctx context.Context, cmd *cobra.Command, idText string) (*report.Document, func(), error) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("历史记录 id 必须是数字，实际 %q", idText)
	}
	repo, closeFn, err := openRepo(cmd)
	if err != nil {
		return nil, nil, err
	}
	rec, err := repo.Get(ctx, id)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	var doc report.Document
	if err := json.Unmarshal(rec.Payload, &doc); err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("解析历史记录失败: %w", err)
	}
	return &doc, closeFn, nil
}

// renderHistoryList 用 tabwriter 渲染历史摘要表。
func renderHistoryList(out io.Writer, summaries []storage.RecordSummary) {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t时间\t命名空间\t对象\t发现数\t最高严重度")
	for _, s := range summaries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\n",
			s.ID, s.CreatedAt.Format("2006-01-02 15:04:05"), s.Namespace,
			s.TargetDisplay, s.FindingsCount, s.HighestSeverity)
	}
	w.Flush()
}

// normalizeFormat 统一格式别名；空值默认 markdown。
func normalizeFormat(format string) string {
	switch format {
	case "md":
		return "markdown"
	case "", "markdown", "html":
		if format == "" {
			return "markdown"
		}
		return format
	default:
		return format
	}
}

func init() {
	historyListCmd.Flags().String("format", "", "输出格式: markdown、html")
	historyShowCmd.Flags().String("format", "", "输出格式: markdown、html")
	historyExportCmd.Flags().StringP("output", "o", "", "导出文件路径")
	historyExportCmd.Flags().String("format", "", "强制指定格式: markdown、html")
	historyCmd.AddCommand(historyListCmd, historyShowCmd, historyExportCmd)
	rootCmd.AddCommand(historyCmd)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/cli/`
Expected: PASS

- [ ] **Step 5: 全包测试并提交**

Run: `go test ./...`
Expected: PASS

```bash
git add internal/cli/history.go internal/cli/history_test.go
git commit -m "feat(cli): history list/show/export 诊断历史命令"
```

---

### Task 15: main 入口接线与文档更新

**Files:**
- Modify: `cmd/opsagent/main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `cli.Execute(ctx)`（Task 13）、`observability.Init(ctx)`（Task 11）

- [ ] **Step 1: 重写 main.go**

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opsagent/internal/cli"
	"opsagent/internal/observability"
)

func main() {
	// Ctrl+C 时取消进行中的采集与调用。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// OTel 未配置时 Init 为 noop。
	shutdown, err := observability.Init(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 追踪初始化失败（%v），继续执行\n", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(flushCtx)
	}()

	if err := cli.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **Step 2: 编译与全包测试**

Run: `go build ./... && go test ./...`
Expected: 编译通过，全部测试 PASS

- [ ] **Step 3: 更新 README**

在 README.md 的「LLM 根因分析」章节之后、文档章节之前插入：

```markdown
## 指标与历史记录

诊断时自动查询 Prometheus 指标（内存工作集/limit、CPU 使用与限流），并把每次诊断保存到本地 SQLite：

\`\`\`bash
# 指定 Prometheus（默认取 OPSAGENT_PROMETHEUS_URL，集群内自动探测）
opsagent diagnose pod/payment-api-xxx -n production --prometheus-url http://prometheus:9090

# 查看历史、复现报告、导出 HTML
opsagent history list
opsagent history show 1
opsagent history export 1 -o report.html

# 最近一次诊断
opsagent diagnose --last
\`\`\`

| 变量 | 说明 |
|---|---|
| `OPSAGENT_PROMETHEUS_URL` | Prometheus 地址 |
| `OPSAGENT_GRAFANA_URL` | Grafana 地址，生成 Explore 链接 |
| `OPSAGENT_HISTORY_DB` | SQLite 文件路径（默认用户配置目录下 opsagent/history.db） |

相关环境变量（`OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_SERVICE_NAME` 等）遵循 OpenTelemetry 标准；未配置时追踪完全关闭。加 `--export report.md` 或 `--export report.html` 可直接导出报告。
```

- [ ] **Step 4: 手动冒烟验证（无集群时验证降级路径）**

Run:

```bash
go run ./cmd/opsagent diagnose --last
```

Expected: 退出并提示「暂无诊断历史」（证明 history/DB 链路可运行）。

Run:

```bash
go run ./cmd/opsagent history list
```

Expected: 空表（仅表头），退出码 0。

- [ ] **Step 5: 提交**

```bash
git add cmd/opsagent/main.go README.md
git commit -m "feat: main 接入信号处理与 OTel 生命周期，更新 README"
```

---

## 验收对照（实施完成后逐条核对）

1. 配置 Prometheus 后 OOM 诊断附带内存趋势与 graph 链接 → Task 5、13；
2. 内存 ≥85%、CPU 限流 ≥25% 独立产出发现 → Task 5；
3. Prometheus/OTel/DB 不可用时规则报告照常且带 notice → Task 12、13；
4. `history list`、`history show`、`--last` 完整复现 → Task 10、13、14；
5. `diagnose --export`、`history export` 支持 .md/.html → Task 7、8、13、14；
6. LLM 编造 ev-x 被标记无效 → Task 6；
7. 无任何 K8s 写操作 → 全部任务；
8. `go test ./...` 通过 → 每个任务。
