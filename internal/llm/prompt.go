package llm

import (
	"fmt"
	"strings"

	"opsagent/internal/diagnose"
	"opsagent/internal/policy"
)

// 输出预算：控制单条证据细节和证据总量，避免日志把上下文撑爆。
const (
	maxDetailPerEvidence = 1500
	maxTotalEvidence     = 12000
)

// systemPrompt 限定模型的角色和边界：只能基于给定材料分析、不许臆测、
// 不提供危险写操作建议、输出固定结构。
const systemPrompt = `你是一名严谨的 Kubernetes 运维诊断助手。你的任务是基于"规则引擎的确定性发现"和"采集到的证据"，汇总根因候选并给出分析。

必须遵守：
1. 只能使用下面提供的材料，不得编造不存在的事件、指标或配置；证据不足时明确说明。
2. 不要建议执行删除资源、强制重启、绕过权限等危险写操作；可以建议进一步采集哪些只读信息。
3. 结论要能对应到具体证据编号（ev-x）。
4. 使用中文，输出严格使用以下 Markdown 结构，不要加与诊断无关的内容：

## 根因分析
按可能性从高到低列出候选根因，每条包含：判断、置信度（高/中/低）、依据的证据编号、推理过程。

## 补充观察
规则没有单独成条、但值得注意的现象（如多个异常之间的关联）。

## 建议进一步确认
为了收敛根因，还需要查看的只读信息或监控指标。`

// BuildPrompt 根据快照、规则发现和证据构建 system/user 提示词。
// 所有进入提示词的文本都先经过脱敏。
func BuildPrompt(snap *diagnose.Snapshot, findings []diagnose.Finding, evidence []diagnose.Evidence) (system, user string) {
	var b strings.Builder

	target := "全部 Pod"
	if snap.Target.Name != "" {
		kind := snap.Target.Kind
		if kind == "" {
			kind = "pod"
		}
		target = fmt.Sprintf("%s/%s", kind, snap.Target.Name)
	}

	b.WriteString("## 诊断对象\n\n")
	b.WriteString(fmt.Sprintf("- 类型与名称: %s\n", target))
	b.WriteString(fmt.Sprintf("- 命名空间: %s\n", snap.Target.Namespace))
	b.WriteString(fmt.Sprintf("- 采集时间: %s\n\n", snap.CollectedAt.Format("2006-01-02 15:04:05")))

	b.WriteString("## 规则引擎的确定性发现\n\n")
	if len(findings) == 0 {
		b.WriteString("规则引擎未发现确定性异常。\n\n")
	}
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("%d. [%s] %s（对象: %s）\n", i+1, f.Severity, f.Title, f.Subject))
		b.WriteString(fmt.Sprintf("   - 分析: %s\n", f.Explanation))
		b.WriteString(fmt.Sprintf("   - 建议: %s\n", f.Suggestion))
		b.WriteString(fmt.Sprintf("   - 证据: %s\n", strings.Join(f.EvidenceIDs, ", ")))
	}

	b.WriteString("\n## 证据明细\n\n")
	total := 0
	for _, e := range evidence {
		detail := truncateDetail(e.Detail)
		section := fmt.Sprintf("### %s [%s] %s\n%s\n\n",
			e.ID, e.Kind, e.Summary, detail)
		if total+len(section) > maxTotalEvidence {
			b.WriteString("（其余证据已因长度预算省略，编号仍可在上文引用）\n")
			break
		}
		b.WriteString(section)
		total += len(section)
	}

	return systemPrompt, policy.SanitizeText(b.String())
}

// truncateDetail 限制单条证据细节长度，超出时保留前部并标注省略。
func truncateDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if len(detail) <= maxDetailPerEvidence {
		return detail
	}
	return detail[:maxDetailPerEvidence] + "\n…（内容已截断）"
}
