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
