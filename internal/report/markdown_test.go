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
