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
