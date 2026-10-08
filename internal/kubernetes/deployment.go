package kubernetes

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DeploymentStatus 是一个 Deployment 的诊断摘要。
type DeploymentStatus struct {
	Name     string
	Strategy string
	Replicas DeploymentReplicas
	Selector string
	// Conditions 只保留非正常的状况：Progressing=False（发布卡住）、ReplicaFailure=True（新 Pod 造不出来）。
	Conditions []DeploymentCondition
}

// DeploymentReplicas 汇总副本计数。
type DeploymentReplicas struct {
	Desired   int32
	Updated   int32
	Ready     int32
	Available int32
}

// DeploymentCondition 是 Deployment 的一条状况。
type DeploymentCondition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// GetDeployment 查询指定 Deployment 并返回摘要。
func GetDeployment(ctx context.Context, client kubernetes.Interface, namespace, name string) (*DeploymentStatus, error) {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("获取 Deployment %q 失败: %w", name, err)
	}
	return summarizeDeployment(deployment), nil
}

// ListDeploymentPods 查询属于指定 Deployment 的 Pod（按 label selector 关联）。
func ListDeploymentPods(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]PodStatus, error) {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("获取 Deployment %q 失败: %w", name, err)
	}

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("解析 Deployment %q 的 selector 失败: %w", name, err)
	}

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("列出 Deployment %q 的 Pod 失败: %w", name, err)
	}

	result := make([]PodStatus, 0, len(pods.Items))
	for i := range pods.Items {
		result = append(result, summarizePod(&pods.Items[i]))
	}
	return result, nil
}

// ListDeploymentWarningEvents 查询与 Deployment 直接相关的 Warning 事件
// （如 FailedCreate、ExceededQuota）。
func ListDeploymentWarningEvents(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]WarningEvent, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name + ",type=Warning",
	})
	if err != nil {
		return nil, fmt.Errorf("查询 Deployment %q 的事件失败: %w", name, err)
	}
	return summarizeEvents(events.Items), nil
}

// desiredReplicas 取期望副本数；Spec.Replicas 为 nil 时 Deployment 默认是 1。
func desiredReplicas(p *int32) int32 {
	if p != nil {
		return *p
	}
	return 1
}

// summarizeDeployment 把 API 对象压缩成诊断摘要，只保留异常状况。
func summarizeDeployment(d *appsv1.Deployment) *DeploymentStatus {
	status := &DeploymentStatus{
		Name:     d.Name,
		Strategy: string(d.Spec.Strategy.Type),
		Replicas: DeploymentReplicas{
			Desired:   desiredReplicas(d.Spec.Replicas),
			Updated:   d.Status.UpdatedReplicas,
			Ready:     d.Status.ReadyReplicas,
			Available: d.Status.AvailableReplicas,
		},
	}
	if d.Spec.Selector != nil {
		selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err == nil {
			status.Selector = selector.String()
		}
	}

	for _, c := range d.Status.Conditions {
		// Progressing=False 表示发布卡住；ReplicaFailure=True 表示副本创建失败。
		if c.Type == appsv1.DeploymentProgressing && c.Status == "True" {
			continue
		}
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status != "True" {
			continue
		}
		if c.Type == appsv1.DeploymentAvailable && c.Status == "True" {
			continue
		}
		status.Conditions = append(status.Conditions, DeploymentCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return status
}
