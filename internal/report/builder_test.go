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
	if len(doc.Notices) != 2 || doc.Notices[0] != "一条提示" {
		t.Errorf("notice 未保留: %v", doc.Notices)
	}
	if !stringsContains(doc.Notices[1], "ev-9") || !stringsContains(doc.Notices[1], "不存在的证据编号") {
		t.Errorf("第 2 条应为无效证据编号警告，实际 %q", doc.Notices[1])
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
