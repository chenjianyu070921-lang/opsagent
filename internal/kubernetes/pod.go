package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodStatus 是一个 Pod 的诊断摘要，只保留排障关心的字段。
type PodStatus struct {
	Name       string
	Phase      string
	NodeName   string
	Conditions []PodCondition
	Containers []ContainerStatus
}

// PodCondition 是 Pod 层面的状况，调度失败等 Pending 原因主要从这里取。
type PodCondition struct {
	Type    string
	Status  string // True / False / Unknown
	Reason  string
	Message string
}

// ContainerStatus 是单个容器的运行状态，Restarts 和异常 Reason 是高频故障信号。
type ContainerStatus struct {
	Name     string
	Ready    bool
	Restarts int32
	// Reason 来自 Waiting/Terminated 状态，如 CrashLoopBackOff、ImagePullBackOff、Error
	Reason  string
	Message string
	// 以下字段来自 Spec，用于规则引擎关联镜像、资源和探针配置。
	Image       string
	MemoryLimit string
	Liveness    *ProbeInfo
	Readiness   *ProbeInfo
	// LastTerminated 是上一次容器退出的信息，OOMKilled / exitCode=137 靠它识别。
	LastTerminated *Termination
}

// ProbeInfo 描述一个探针的类型与目标。
type ProbeInfo struct {
	Kind   string // http-get / tcp / exec
	Target string // :8080/health、:8080、["cmd","arg"]
}

// Termination 是一次容器终止的摘要。
type Termination struct {
	Reason     string
	ExitCode   int32
	FinishedAt time.Time
}

// WarningEvent 是一条与诊断对象相关的 Warning 级别事件。
type WarningEvent struct {
	Reason        string
	Message       string
	Count         int32
	LastTimestamp time.Time
}

// ListPods 返回命名空间内的 Pod 摘要；podName 非空时只查询该 Pod。
// 入参用 kubernetes.Interface 而非具体的 Clientset，方便以后写单元测试时传入假实现。
func ListPods(ctx context.Context, client kubernetes.Interface, namespace, podName string) ([]PodStatus, error) {
	if podName != "" {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("获取 Pod %q 失败: %w", podName, err)
		}
		return []PodStatus{summarizePod(pod)}, nil
	}

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("列出命名空间 %q 的 Pod 失败: %w", namespace, err)
	}

	result := make([]PodStatus, 0, len(pods.Items))
	for i := range pods.Items {
		result = append(result, summarizePod(&pods.Items[i]))
	}
	return result, nil
}

// ListPodWarningEvents 查询与指定 Pod 相关的 Warning 事件（相当于 kubectl describe 里的事件区）。
func ListPodWarningEvents(ctx context.Context, client kubernetes.Interface, namespace, podName string) ([]WarningEvent, error) {
	// FieldSelector 在服务端过滤，只取回"这个 Pod 的 Warning 事件"，避免拉全量事件再筛选。
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + podName + ",type=Warning",
	})
	if err != nil {
		return nil, fmt.Errorf("查询 Pod %q 的事件失败: %w", podName, err)
	}
	return summarizeEvents(events.Items), nil
}

// summarizePod 把 API 返回的庞大 Pod 对象压缩成排障需要的摘要。
func summarizePod(pod *v1.Pod) PodStatus {
	status := PodStatus{
		Name:     pod.Name,
		Phase:    string(pod.Status.Phase),
		NodeName: pod.Spec.NodeName,
	}

	for _, c := range pod.Status.Conditions {
		// Ready / ContainersReady 是从容器状态推导出来的派生状况，只描述结果，
		// 对定位原因没有价值，统一丢弃。
		if c.Type == v1.PodReady || c.Type == v1.ContainersReady {
			continue
		}
		// 只保留有信息量的状况；正常的 True 状况对排障没有价值。
		if c.Status == v1.ConditionTrue {
			continue
		}
		status.Conditions = append(status.Conditions, PodCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	// Spec 和 Status 的容器列表按名字合并：Pending 的 Pod 可能还没有任何 ContainerStatus。
	statusByIndex := make(map[string]int)
	for i := range pod.Spec.Containers {
		c := pod.Spec.Containers[i]
		container := ContainerStatus{
			Name:        c.Name,
			Image:       c.Image,
			MemoryLimit: memoryLimitString(c.Resources.Limits),
			Liveness:    probeInfo(c.LivenessProbe),
			Readiness:   probeInfo(c.ReadinessProbe),
		}
		statusByIndex[c.Name] = len(status.Containers)
		status.Containers = append(status.Containers, container)
	}

	for _, cs := range pod.Status.ContainerStatuses {
		idx, ok := statusByIndex[cs.Name]
		if !ok {
			// 理论上不会发生（status 容器都在 spec 里），兜底保留。
			idx = len(status.Containers)
			status.Containers = append(status.Containers, ContainerStatus{Name: cs.Name})
		}
		container := status.Containers[idx]
		container.Ready = cs.Ready
		container.Restarts = cs.RestartCount

		// State 三选一：Waiting（启动失败/反复重启）、Running、Terminated（已退出）
		switch {
		case cs.State.Waiting != nil:
			container.Reason = cs.State.Waiting.Reason
			container.Message = cs.State.Waiting.Message
		case cs.State.Terminated != nil:
			container.Reason = cs.State.Terminated.Reason
			container.Message = cs.State.Terminated.Message
		}

		// LastTerminationState 记录上一次退出；正常启动的容器该字段为 nil。
		if t := cs.LastTerminationState.Terminated; t != nil {
			container.LastTerminated = &Termination{
				Reason:     t.Reason,
				ExitCode:   t.ExitCode,
				FinishedAt: t.FinishedAt.Time,
			}
		}
		status.Containers[idx] = container
	}
	return status
}

// summarizeEvents 把事件列表压缩成摘要。
func summarizeEvents(items []v1.Event) []WarningEvent {
	result := make([]WarningEvent, 0, len(items))
	for _, e := range items {
		result = append(result, WarningEvent{
			Reason:        e.Reason,
			Message:       e.Message,
			Count:         e.Count,
			LastTimestamp: e.LastTimestamp.Time,
		})
	}
	return result
}

// memoryLimitString 提取内存 limit 的人类可读表示，未设置时返回空串。
func memoryLimitString(limits v1.ResourceList) string {
	if mem, ok := limits[v1.ResourceMemory]; ok {
		return mem.String()
	}
	return ""
}

// probeInfo 把探针配置压缩成"类型 + 目标"，未配置时返回 nil。
func probeInfo(p *v1.Probe) *ProbeInfo {
	if p == nil {
		return nil
	}
	switch {
	case p.HTTPGet != nil:
		h := p.HTTPGet
		path := h.Path
		if path == "" {
			path = "/"
		}
		scheme := ""
		if h.Scheme == v1.URISchemeHTTPS {
			scheme = "https://"
		}
		return &ProbeInfo{Kind: "http-get", Target: fmt.Sprintf("%s:%d%s", scheme, h.Port.IntValue(), path)}
	case p.TCPSocket != nil:
		return &ProbeInfo{Kind: "tcp", Target: fmt.Sprintf(":%d", p.TCPSocket.Port.IntValue())}
	case p.Exec != nil:
		return &ProbeInfo{Kind: "exec", Target: "[" + strings.Join(p.Exec.Command, " ") + "]"}
	default:
		return nil
	}
}
