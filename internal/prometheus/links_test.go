package prometheus_test

import (
	"encoding/json"
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
	// left 是 url.QueryEscape 编码的 JSON（见设计文档 4.3），
	// 内层引号按 JSON 规则转义，因此解析后应原样取回 PromQL。
	var state struct {
		Queries []struct {
			Expr string `json:"expr"`
		} `json:"queries"`
	}
	if err := json.Unmarshal([]byte(u.Query().Get("left")), &state); err != nil {
		t.Fatalf("left 不是合法 JSON: %v", err)
	}
	if len(state.Queries) != 1 || state.Queries[0].Expr != `up{namespace="default"}` {
		t.Errorf("left 未原样携带 PromQL: %+v", state.Queries)
	}
}
