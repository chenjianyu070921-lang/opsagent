package diagnose

import (
	"fmt"
	"sort"
	"strings"

	"opsagent/internal/kubernetes"
)

// Severity 表示发现的严重程度。
type Severity string

const (
	SeverityCritical Severity = "严重"
	SeverityHigh     Severity = "高"
	SeverityMedium   Severity = "中"
	SeverityLow      Severity = "低"
)

// Finding 是规则引擎产出的一条确定性发现，必须引用至少一条证据。
type Finding struct {
	Title       string
	Category    string
	Severity    Severity
	Subject     string // pod/xxx、pod/xxx container/yyy、deployment/xxx
	Explanation string
	Suggestion  string
	Risk        string
	EvidenceIDs []string
}

// Rule 是一条诊断规则：基于快照产出发现，并通过证据集注册引用证据。
type Rule func(*Snapshot, *EvidenceSet) []Finding

// rules 按优先级排列具体规则；PodNotReady 兜底规则需要"已覆盖 Pod"集合，
// 由 RunRules 用闭包单独编排。
var rules = []Rule{
	crashLoopBackOffRule,
	imagePullBackOffRule,
	oomKilledRule,
	pendingPodRule,
	probeFailureRule,
	deploymentRule,
}

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
	sort.SliceStable(findings, func(i, j int) bool {
		return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
	})
	return findings, evidence.List()
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	default:
		return 3
	}
}

// --- 工具函数 ---

func podSource(name string) string { return "pod/" + name }

func containerSource(podName, containerName string) string {
	return fmt.Sprintf("pod/%s container/%s", podName, containerName)
}

func subjectPodName(subject string) string {
	rest := strings.TrimPrefix(subject, "pod/")
	return strings.SplitN(rest, " container/", 2)[0]
}

// statusDetail 拼出容器的状态细节。
func containerStatusDetail(c kubernetes.ContainerStatus) string {
	lines := []string{
		fmt.Sprintf("镜像: %s", c.Image),
		fmt.Sprintf("重启次数: %d", c.Restarts),
	}
	if c.Reason != "" {
		lines = append(lines, fmt.Sprintf("当前状态: %s %s", c.Reason, c.Message))
	}
	if c.LastTerminated != nil {
		t := c.LastTerminated
		lines = append(lines, fmt.Sprintf("上次退出: reason=%s exitCode=%d finishedAt=%s",
			t.Reason, t.ExitCode, t.FinishedAt.Format("2006-01-02 15:04:05")))
	}
	if c.MemoryLimit != "" {
		lines = append(lines, fmt.Sprintf("内存 limit: %s", c.MemoryLimit))
	}
	return strings.Join(lines, "\n")
}

// addLogEvidence 把容器日志注册为证据（存在时）。
func addLogEvidence(set *EvidenceSet, snap *Snapshot, podName, containerName string) (string, bool) {
	if containers, ok := snap.Logs[podName]; ok {
		if log, ok := containers[containerName]; ok {
			summary := fmt.Sprintf("容器 %s 的最近日志（%d 行）", containerName, log.TailLines)
			id := set.Add("log", containerSource(podName, containerName), summary, log.Content)
			return id, true
		}
	}
	return "", false
}

// --- 规则 1：CrashLoopBackOff ---

func crashLoopBackOffRule(snap *Snapshot, set *EvidenceSet) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		for _, c := range pod.Containers {
			if c.Reason != "CrashLoopBackOff" && c.Restarts < 3 {
				continue
			}

			summary := fmt.Sprintf("容器 %s 反复崩溃重启（已重启 %d 次）", c.Name, c.Restarts)
			ids := []string{set.Add("status", containerSource(pod.Name, c.Name), summary, containerStatusDetail(c))}
			if id, ok := addLogEvidence(set, snap, pod.Name, c.Name); ok {
				ids = append(ids, id)
			}

			exitInfo := ""
			if c.LastTerminated != nil {
				exitInfo = fmt.Sprintf("，上次退出码 %d（%s）", c.LastTerminated.ExitCode, c.LastTerminated.Reason)
			}
			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 处于 CrashLoopBackOff", c.Name),
				Category:    "crashloop",
				Severity:    SeverityCritical,
				Subject:     containerSource(pod.Name, c.Name),
				Explanation: fmt.Sprintf("容器启动后反复异常退出%s，Kubernetes 已进入指数退避重启。", exitInfo),
				Suggestion:  "查看下方日志定位进程退出原因；重点检查启动命令、配置/环境变量、依赖服务（DB/缓存）连通性、探针配置与启动耗时。",
				Risk:        "容器持续不可用会导致该 Pod 不接流量，重启风暴还会反复消耗节点资源；在根因消除前不要简单依赖自动重启恢复。",
				EvidenceIDs: ids,
			})
		}
	}
	return findings
}

// --- 规则 2：ImagePullBackOff ---

func imagePullBackOffRule(snap *Snapshot, set *EvidenceSet) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		for _, c := range pod.Containers {
			switch c.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError":
			default:
				continue
			}

			summary := fmt.Sprintf("容器 %s 镜像拉取失败：%s", c.Name, c.Image)
			ids := []string{set.Add("status", containerSource(pod.Name, c.Name), summary, containerStatusDetail(c))}

			// 关联拉取相关事件（Failed to pull image、仓库认证失败等）。
			for _, e := range snap.Events[pod.Name] {
				if strings.Contains(e.Message, "pull") || strings.Contains(e.Message, c.Image) {
					ids = append(ids, set.Add("event", podSource(pod.Name),
						fmt.Sprintf("%s: %s (x%d)", e.Reason, e.Message, e.Count), e.Message))
				}
			}

			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 镜像拉取失败（%s）", c.Name, c.Reason),
				Category:    "imagepull",
				Severity:    SeverityHigh,
				Subject:     containerSource(pod.Name, c.Name),
				Explanation: fmt.Sprintf("节点无法拉取镜像 %s：%s。", c.Image, c.Message),
				Suggestion:  "确认镜像名和 tag 是否存在、拼写是否正确；确认 imagePullSecrets 与仓库账号权限；检查节点到镜像仓库的网络。",
				Risk:        "镜像一天不修复 Pod 就一天无法启动；不要通过随意放宽仓库权限或复用他人凭证来临时绕过。",
				EvidenceIDs: ids,
			})
		}
	}
	return findings
}

// --- 规则 3：OOMKilled ---

func oomKilledRule(snap *Snapshot, set *EvidenceSet) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		for _, c := range pod.Containers {
			t := c.LastTerminated
			if t == nil || (t.Reason != "OOMKilled" && t.ExitCode != 137) {
				continue
			}

			detail := containerStatusDetail(c)
			summary := fmt.Sprintf("容器 %s 因内存超限被杀（exitCode=137）", c.Name)
			ids := []string{set.Add("status", containerSource(pod.Name, c.Name), summary, detail)}
			if id, ok := addLogEvidence(set, snap, pod.Name, c.Name); ok {
				ids = append(ids, id)
			}

			limitInfo := "未设置内存 limit"
			if c.MemoryLimit != "" {
				limitInfo = "当前内存 limit 为 " + c.MemoryLimit
			}
			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 被 OOM Kill", c.Name),
				Category:    "oom",
				Severity:    SeverityCritical,
				Subject:     containerSource(pod.Name, c.Name),
				Explanation: "容器内存使用超过 limit，被内核 OOM Killer 终止，Kubernetes 随后将其重启。",
				Suggestion:  fmt.Sprintf("%s。先结合监控判断是内存持续增长（排查泄漏、堆配置、大对象/缓存）还是真实业务增长，确认后再调整 limit/requests 或为程序增加内存。", limitInfo),
				Risk:        "不查根因直接调高 limit 只会推迟崩溃，并会挤占同节点其他 Pod 的可用内存，可能诱发节点级问题。",
				EvidenceIDs: ids,
			})
		}
	}
	return findings
}

// --- 规则 4：Pending（调度失败）---

func pendingPodRule(snap *Snapshot, set *EvidenceSet) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if pod.Phase != "Pending" {
			continue
		}

		// 只有确实没被调度（PodScheduled=False 或有 FailedScheduling 事件）时才报调度问题；
		// 已调度但卡在镜像拉取/容器创建的 Pending 由对应规则覆盖。
		var conditionText []string
		for _, cond := range pod.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				if cond.Reason != "" || cond.Message != "" {
					conditionText = append(conditionText, fmt.Sprintf("%s: %s", cond.Reason, cond.Message))
				}
			}
		}

		hasFailedScheduling := false
		for _, e := range snap.Events[pod.Name] {
			if e.Reason == "FailedScheduling" {
				hasFailedScheduling = true
			}
		}
		if len(conditionText) == 0 && !hasFailedScheduling {
			continue
		}

		ids := []string{set.Add("status", podSource(pod.Name),
			"Pod 处于 Pending，无法被调度",
			strings.Join(conditionText, "\n"))}

		for _, e := range snap.Events[pod.Name] {
			if e.Reason == "FailedScheduling" {
				ids = append(ids, set.Add("event", podSource(pod.Name),
					fmt.Sprintf("%s: %s (x%d)", e.Reason, e.Message, e.Count), e.Message))
			}
		}

		joined := strings.ToLower(strings.Join(conditionText, " ") + " " +
			eventMessages(snap.Events[pod.Name]))
		hint, suggestion := schedulingHint(joined)

		findings = append(findings, Finding{
			Title:       fmt.Sprintf("Pod %s 无法调度（Pending）", pod.Name),
			Category:    "pending",
			Severity:    SeverityHigh,
			Subject:     podSource(pod.Name),
			Explanation: "Pod 已创建但没有节点能满足调度条件" + hint,
			Suggestion:  suggestion,
			Risk:        "该副本持续缺失会降低服务容量；放宽节点选择或污点容忍前，需确认目标节点确实能承载。",
			EvidenceIDs: ids,
		})
	}
	return findings
}

func eventMessages(events []kubernetes.WarningEvent) string {
	messages := make([]string, 0, len(events))
	for _, e := range events {
		messages = append(messages, e.Message)
	}
	return strings.Join(messages, " ")
}

// schedulingHint 根据调度失败文本判断具体原因并给出对应建议。
func schedulingHint(text string) (hint, suggestion string) {
	switch {
	case strings.Contains(text, "insufficient"):
		return "：集群资源不足", "检查节点 CPU/内存余量和 Pod requests；可扩容节点、降低 requests，或清理挤占资源的工作负载。"
	case strings.Contains(text, "taint"):
		return "：节点污点不匹配", "确认目标节点污点，必要时为 Pod 增加对应的 tolerations。"
	case strings.Contains(text, "persistentvolumeclaim") || strings.Contains(text, "pvc") || strings.Contains(text, "volume"):
		return "：存储卷未就绪", "检查 PVC 是否已绑定、StorageClass 和卷供应是否正常。"
	case strings.Contains(text, "anti-affinity"):
		return "：Pod 反亲和规则无法满足", "检查 podAntiAffinity 规则与可用拓扑域数量，必要时调整约束或扩容拓扑域。"
	default:
		return "", "查看调度事件中的具体原因，依次排查节点资源余量、污点容忍、亲和性、PVC 和配额（ResourceQuota）。"
	}
}

// --- 规则 5：探针失败 ---

func probeFailureRule(snap *Snapshot, set *EvidenceSet) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		// 按探针类型聚合事件，避免同一次失败刷出多条发现。
		var livenessEvents, readinessEvents []kubernetes.WarningEvent
		for _, e := range snap.Events[pod.Name] {
			if e.Reason != "Unhealthy" && e.Reason != "ProbeWarning" {
				continue
			}
			switch {
			case strings.Contains(e.Message, "Liveness") || strings.Contains(e.Message, "liveness"):
				livenessEvents = append(livenessEvents, e)
			case strings.Contains(e.Message, "Readiness") || strings.Contains(e.Message, "readiness"):
				readinessEvents = append(readinessEvents, e)
			}
		}

		if f := probeFinding(pod.Name, "liveness", "存活探针", livenessEvents, set, snap); f != nil {
			findings = append(findings, *f)
		}
		if f := probeFinding(pod.Name, "readiness", "就绪探针", readinessEvents, set, snap); f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

func probeFinding(podName, probeKind, probeLabel string, events []kubernetes.WarningEvent, set *EvidenceSet, snap *Snapshot) *Finding {
	if len(events) == 0 {
		return nil
	}

	ids := make([]string, 0, len(events)+1)
	for _, e := range events {
		ids = append(ids, set.Add("event", podSource(podName),
			fmt.Sprintf("%s: %s (x%d)", e.Reason, e.Message, e.Count), e.Message))
	}

	// 找到配置了该探针的容器，把探针目标作为配置证据。
	var target string
	for _, pod := range snap.Pods {
		if pod.Name != podName {
			continue
		}
		for _, c := range pod.Containers {
			var p *kubernetes.ProbeInfo
			if probeKind == "liveness" {
				p = c.Liveness
			} else {
				p = c.Readiness
			}
			if p != nil {
				target = fmt.Sprintf("容器 %s %s: %s %s", c.Name, probeLabel, p.Kind, p.Target)
				ids = append(ids, set.Add("status", containerSource(podName, c.Name),
					probeLabel+"配置: "+p.Kind+" "+p.Target, target))
			}
		}
	}

	finding := &Finding{
		Title:       fmt.Sprintf("Pod %s %s失败", podName, probeLabel),
		Category:    "probe",
		Severity:    SeverityMedium,
		Subject:     podSource(podName),
		Explanation: fmt.Sprintf("%s连续探测失败：%s", probeLabel, events[0].Message),
		Suggestion:  "检查应用对应端口和检查路径是否正常、应用启动耗时是否超过 initialDelaySeconds、下游依赖是否拖慢健康检查；必要时结合日志确认进程状态。",
		EvidenceIDs: ids,
	}
	if probeKind == "liveness" {
		finding.Risk = "存活探针持续失败会导致容器被反复杀掉重启，表现与 CrashLoopBackOff 类似。"
	} else {
		finding.Risk = "就绪探针失败期间 Pod 不会被加入 Endpoints，Service 流量不会分发到该 Pod。"
	}
	return finding
}

// --- 规则 6：Deployment 发布失败 ---

func deploymentRule(snap *Snapshot, set *EvidenceSet) []Finding {
	if snap.Deployment == nil {
		return nil
	}

	var findings []Finding
	for _, cond := range snap.Deployment.Conditions {
		var title, explanation string
		switch {
		case cond.Type == "Progressing" && cond.Status == "False":
			title = "Deployment 发布卡住（ProgressDeadlineExceeded）"
			explanation = fmt.Sprintf("Deployment 在规定时间内未能完成新版本滚动：%s", cond.Message)
		case cond.Type == "ReplicaFailure" && cond.Status == "True":
			title = "Deployment 副本创建失败（ReplicaFailure）"
			explanation = fmt.Sprintf("新版本 ReplicaSet 无法创建 Pod：%s", cond.Message)
		default:
			continue
		}

		source := "deployment/" + snap.Deployment.Name
		ids := []string{set.Add("deployment", source, title, cond.Message)}
		for _, e := range snap.DeploymentEvents {
			ids = append(ids, set.Add("event", source,
				fmt.Sprintf("%s: %s (x%d)", e.Reason, e.Message, e.Count), e.Message))
		}

		findings = append(findings, Finding{
			Title:       title,
			Category:    "deployment",
			Severity:    SeverityHigh,
			Subject:     source,
			Explanation: explanation,
			Suggestion:  "根据 Deployment 事件定位原因（ResourceQuota 不足、镜像拉取失败、调度失败、webhook 拦截等），并参考下方关联 Pod 的具体诊断发现。",
			Risk:        "新版本无法上线期间服务只能依赖旧副本，容量和回滚窗口都受限；未确认新 Pod 正常前不要继续扩大发布。",
			EvidenceIDs: ids,
		})
	}
	return findings
}

// --- 规则 7：兜底——Pod 未就绪 ---

func podNotReadyRule(snap *Snapshot, set *EvidenceSet, covered map[string]bool) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if covered[pod.Name] || pod.Phase == "Succeeded" {
			continue
		}

		abnormal := pod.Phase != "Running"
		var notReady []string
		for _, c := range pod.Containers {
			if !c.Ready {
				notReady = append(notReady, c.Name)
				abnormal = true
			}
		}
		if !abnormal {
			continue
		}

		summary := fmt.Sprintf("Pod 非正常运行（phase=%s），未就绪容器: %s",
			pod.Phase, strings.Join(notReady, ", "))
		lines := conditionText(pod)
		for _, c := range pod.Containers {
			if c.Reason != "" {
				lines = append(lines, fmt.Sprintf("容器 %s: %s %s", c.Name, c.Reason, c.Message))
			}
		}
		detail := strings.Join(lines, "\n")
		ids := []string{set.Add("status", podSource(pod.Name), summary, detail)}
		if len(snap.Events[pod.Name]) > 0 {
			for _, e := range snap.Events[pod.Name] {
				ids = append(ids, set.Add("event", podSource(pod.Name),
					fmt.Sprintf("%s: %s (x%d)", e.Reason, e.Message, e.Count), e.Message))
			}
		}
		if id, ok := addLogEvidence(set, snap, pod.Name, firstNotReady(pod)); ok {
			ids = append(ids, id)
		}

		findings = append(findings, Finding{
			Title:       fmt.Sprintf("Pod %s 当前未就绪", pod.Name),
			Category:    "notready",
			Severity:    SeverityMedium,
			Subject:     podSource(pod.Name),
			Explanation: "Pod 处于非正常状态且未命中确定性规则，需要结合事件与日志进一步判断。",
			Suggestion:  "查看下方该 Pod 的 Warning 事件和容器日志；若为刚发布的版本，对比上一版本的镜像和配置变更。",
			Risk:        "该 Pod 当前不承载（完整）流量，持续不恢复会降低服务可用容量。",
			EvidenceIDs: ids,
		})
	}
	return findings
}

func conditionText(pod kubernetes.PodStatus) []string {
	lines := make([]string, 0, len(pod.Conditions))
	for _, c := range pod.Conditions {
		lines = append(lines, fmt.Sprintf("%s[%s]: %s %s", c.Type, c.Status, c.Reason, c.Message))
	}
	return lines
}

func firstNotReady(pod kubernetes.PodStatus) string {
	for _, c := range pod.Containers {
		if !c.Ready {
			return c.Name
		}
	}
	return ""
}
