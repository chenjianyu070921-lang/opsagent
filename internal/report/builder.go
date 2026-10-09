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
