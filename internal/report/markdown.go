// Package report 把诊断快照和规则发现渲染成报告。
package report

import (
	"fmt"
	"strings"

	"opsagent/internal/diagnose"
)

// RenderMarkdown 生成 Markdown 诊断报告：结论摘要、发现详情（含建议与风险）、证据链。
func RenderMarkdown(snap *diagnose.Snapshot, findings []diagnose.Finding, evidence []diagnose.Evidence) string {
	var b strings.Builder

	target := "all pods"
	if snap.Target.Name != "" {
		kind := snap.Target.Kind
		if kind == "" {
			kind = "pod"
		}
		target = fmt.Sprintf("%s/%s", kind, snap.Target.Name)
	}

	b.WriteString("# 诊断报告\n\n")
	b.WriteString(fmt.Sprintf("- 诊断对象: %s\n", target))
	b.WriteString(fmt.Sprintf("- 命名空间: %s\n", snap.Target.Namespace))
	b.WriteString(fmt.Sprintf("- 采集时间: %s\n", snap.CollectedAt.Format("2006-01-02 15:04:05")))
	b.WriteString("\n")

	if len(findings) == 0 {
		b.WriteString("## 结论\n\n未发现确定性异常。\n\n")
		writeOverview(&b, snap)
		b.WriteString("\n")
		writeEvidence(&b, evidence)
		return b.String()
	}

	b.WriteString("## 结论摘要\n\n")
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("%d. **[%s]** %s（%s）\n", i+1, f.Severity, f.Title, f.Subject))
	}
	b.WriteString("\n")

	b.WriteString("## 发现详情\n\n")
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("### %d. %s\n\n", i+1, f.Title))
		b.WriteString(fmt.Sprintf("- 严重程度: %s\n", f.Severity))
		b.WriteString(fmt.Sprintf("- 对象: %s\n", f.Subject))
		b.WriteString(fmt.Sprintf("- 分析: %s\n", f.Explanation))
		b.WriteString(fmt.Sprintf("- 建议: %s\n", f.Suggestion))
		b.WriteString(fmt.Sprintf("- 风险提示: %s\n", f.Risk))
		b.WriteString(fmt.Sprintf("- 证据: %s\n", strings.Join(f.EvidenceIDs, ", ")))
		b.WriteString("\n")
	}

	writeOverview(&b, snap)
	b.WriteString("\n")
	writeEvidence(&b, evidence)
	return b.String()
}

// writeOverview 附加资源概况，让"无异常"时报告也有信息量。
func writeOverview(b *strings.Builder, snap *diagnose.Snapshot) {
	b.WriteString("## 资源概况\n\n")
	if d := snap.Deployment; d != nil {
		b.WriteString(fmt.Sprintf("- Deployment %s: desired=%d updated=%d ready=%d available=%d\n",
			d.Name, d.Replicas.Desired, d.Replicas.Updated, d.Replicas.Ready, d.Replicas.Available))
	}
	for _, pod := range snap.Pods {
		ready := 0
		for _, c := range pod.Containers {
			if c.Ready {
				ready++
			}
		}
		b.WriteString(fmt.Sprintf("- Pod %s: phase=%s ready=%d/%d\n",
			pod.Name, pod.Phase, ready, len(pod.Containers)))
	}
}

// writeEvidence 按类型分组渲染证据链。
func writeEvidence(b *strings.Builder, evidence []diagnose.Evidence) {
	b.WriteString("## 证据链\n\n")
	groups := []struct {
		kind, title string
	}{
		{"deployment", "Deployment 状态"},
		{"status", "资源状态"},
		{"event", "事件"},
		{"log", "日志"},
	}
	for _, g := range groups {
		var items []diagnose.Evidence
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
			b.WriteString(fmt.Sprintf("**[%s] %s — %s**\n\n", e.ID, e.Source, e.Summary))
			if strings.TrimSpace(e.Detail) != "" {
				b.WriteString("```\n")
				b.WriteString(strings.TrimRight(e.Detail, "\n"))
				b.WriteString("\n```\n\n")
			}
		}
	}
}
