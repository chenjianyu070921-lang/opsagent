// Package diagnose 负责诊断编排：采集集群证据构成快照，并由规则引擎产出确定性发现。
package diagnose

import (
	"context"
	"fmt"
	"time"

	k8sclient "k8s.io/client-go/kubernetes"

	"opsagent/internal/kubernetes"
)

// Target 表示一次诊断的对象。
// Kind 为空（或 all）且 Name 为空时，表示诊断命名空间下的全部 Pod。
type Target struct {
	Kind      string // pod / deployment / ""
	Name      string
	Namespace string
}

// Snapshot 是一次诊断采集到的全部现场信息。
type Snapshot struct {
	Target     Target
	Deployment *kubernetes.DeploymentStatus
	// DeploymentEvents 是与 Deployment 自身相关的事件（FailedCreate、ExceededQuota 等）。
	DeploymentEvents []kubernetes.WarningEvent
	Pods             []kubernetes.PodStatus
	// Events 按 Pod 名索引该 Pod 的 Warning 事件。
	Events map[string][]kubernetes.WarningEvent
	// Logs 按 Pod 名 → 容器名索引可疑容器日志。
	Logs        map[string]map[string]kubernetes.ContainerLog
	CollectedAt time.Time
}

// Collect 按诊断对象采集基础资源、事件和可疑容器日志，构成快照。
func Collect(ctx context.Context, client k8sclient.Interface, target Target, since time.Duration, tailLines int64) (*Snapshot, error) {
	snap := &Snapshot{
		Target:      target,
		Events:      make(map[string][]kubernetes.WarningEvent),
		Logs:        make(map[string]map[string]kubernetes.ContainerLog),
		CollectedAt: time.Now(),
	}

	switch target.Kind {
	case "pod":
		pods, err := kubernetes.ListPods(ctx, client, target.Namespace, target.Name)
		if err != nil {
			return nil, err
		}
		snap.Pods = pods
	case "deployment":
		deployment, err := kubernetes.GetDeployment(ctx, client, target.Namespace, target.Name)
		if err != nil {
			return nil, err
		}
		snap.Deployment = deployment

		events, err := kubernetes.ListDeploymentWarningEvents(ctx, client, target.Namespace, target.Name)
		if err != nil {
			return nil, err
		}
		snap.DeploymentEvents = events

		pods, err := kubernetes.ListDeploymentPods(ctx, client, target.Namespace, target.Name)
		if err != nil {
			return nil, err
		}
		snap.Pods = pods
	case "", "all":
		pods, err := kubernetes.ListPods(ctx, client, target.Namespace, "")
		if err != nil {
			return nil, err
		}
		snap.Pods = pods
	default:
		return nil, fmt.Errorf("暂不支持的资源类型 %q，目前支持: pod、deployment", target.Kind)
	}

	if err := collectPodEvidence(ctx, client, snap, since, tailLines); err != nil {
		return nil, err
	}
	return snap, nil
}

// collectPodEvidence 为快照中的每个 Pod 采集 Warning 事件，并为可疑容器采集日志。
func collectPodEvidence(ctx context.Context, client k8sclient.Interface, snap *Snapshot, since time.Duration, tailLines int64) error {
	for _, pod := range snap.Pods {
		events, err := kubernetes.ListPodWarningEvents(ctx, client, snap.Target.Namespace, pod.Name)
		if err != nil {
			return err
		}
		if len(events) > 0 {
			snap.Events[pod.Name] = events
		}

		for _, c := range pod.Containers {
			// 只取可疑容器的日志，控制输出预算：未就绪、有重启、或 Pod 非正常运行。
			if c.Ready && c.Restarts == 0 && pod.Phase == "Running" {
				continue
			}
			log, err := kubernetes.GetContainerLogs(ctx, client, snap.Target.Namespace, pod.Name, c.Name, since, tailLines)
			if err != nil {
				// 日志采集失败（容器尚未启动等）不应让整个诊断失败，跳过该容器即可。
				continue
			}
			if log.Content == "" {
				continue
			}
			if snap.Logs[pod.Name] == nil {
				snap.Logs[pod.Name] = make(map[string]kubernetes.ContainerLog)
			}
			snap.Logs[pod.Name][c.Name] = *log
		}
	}
	return nil
}

// Evidence 是一条可被结论引用的证据。
type Evidence struct {
	ID      string // ev-1、ev-2…，由 EvidenceSet 分配
	Kind    string // status / event / log / deployment
	Source  string // pod/xxx、pod/xxx container/yyy、deployment/xxx
	Summary string
	Detail  string
}

// EvidenceSet 是证据收集器，供规则注册证据并拿回 ID；按 Kind+Source+Summary 去重。
type EvidenceSet struct {
	items []Evidence
	index map[string]string // 去重键 → ID
}

// NewEvidenceSet 创建空证据集。
func NewEvidenceSet() *EvidenceSet {
	return &EvidenceSet{index: make(map[string]string)}
}

// Add 注册一条证据；若内容相同的证据已存在则直接返回其 ID。
func (s *EvidenceSet) Add(kind, source, summary, detail string) string {
	key := kind + "|" + source + "|" + summary
	if id, ok := s.index[key]; ok {
		return id
	}
	id := fmt.Sprintf("ev-%d", len(s.items)+1)
	s.index[key] = id
	s.items = append(s.items, Evidence{
		ID: id, Kind: kind, Source: source, Summary: summary, Detail: detail,
	})
	return id
}

// List 返回按注册顺序排列的全部证据。
func (s *EvidenceSet) List() []Evidence {
	return s.items
}
