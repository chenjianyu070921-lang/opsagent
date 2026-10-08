package llm_test

import (
	"strings"
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/llm"
)

func testSnapshot() (*diagnose.Snapshot, []diagnose.Finding, []diagnose.Evidence) {
	snap := &diagnose.Snapshot{
		Target:      diagnose.Target{Kind: "pod", Name: "p1", Namespace: "default"},
		Pods:        []kubernetes.PodStatus{{Name: "p1", Phase: "Running"}},
		CollectedAt: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
	}

	// 先走一遍规则引擎，拿到真实的发现与证据，保证提示词内容与生产路径一致。
	findings, evidence := diagnose.RunRules(snap)
	return snap, findings, evidence
}

func TestBuildPromptContainsRequiredSections(t *testing.T) {
	snap, findings, evidence := testSnapshot()

	// 人为挂一条发现，验证发现与证据编号会进入提示词。
	findings = append(findings, diagnose.Finding{
		Title:       "测试发现",
		Severity:    diagnose.SeverityHigh,
		Subject:     "pod/p1 container/app",
		Explanation: "测试解释",
		EvidenceIDs: []string{"ev-9"},
	})
	evidence = append(evidence, diagnose.Evidence{
		ID: "ev-9", Kind: "status", Source: "pod/p1", Summary: "测试证据", Detail: "细节",
	})

	system, user := llm.BuildPrompt(snap, findings, evidence)

	for _, want := range []string{"根因分析", "补充观察", "建议进一步确认"} {
		if !strings.Contains(system, want) {
			t.Errorf("system 提示词缺少章节要求 %q", want)
		}
	}
	for _, want := range []string{"pod/p1", "测试发现", "测试解释", "ev-9", "测试证据", "default"} {
		if !strings.Contains(user, want) {
			t.Errorf("user 提示词缺少 %q", want)
		}
	}
}

func TestBuildPromptSanitizesSecrets(t *testing.T) {
	snap, findings, evidence := testSnapshot()
	evidence = append(evidence, diagnose.Evidence{
		ID:      "ev-secret",
		Kind:    "log",
		Source:  "pod/p1 container/app",
		Summary: "日志",
		Detail:  "Authorization: Bearer abcdef-ghijkl-123456",
	})

	_, user := llm.BuildPrompt(snap, findings, evidence)
	if strings.Contains(user, "abcdef-ghijkl-123456") {
		t.Errorf("提示词中仍包含未脱敏的凭证：\n%s", user)
	}
}

func TestBuildPromptTruncatesLongEvidence(t *testing.T) {
	snap, findings, _ := testSnapshot()

	long := strings.Repeat("x", 1500+500)
	evidence := []diagnose.Evidence{{
		ID: "ev-long", Kind: "log", Source: "pod/p1", Summary: "超长日志", Detail: long,
	}}

	_, user := llm.BuildPrompt(snap, findings, evidence)
	if strings.Count(user, "x") > 1500 {
		t.Errorf("单条证据未按预算截断")
	}
	if !strings.Contains(user, "已截断") {
		t.Errorf("超长证据应标注截断")
	}
}
