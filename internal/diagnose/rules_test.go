package diagnose_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
)

func newSnapshot(pods ...kubernetes.PodStatus) *diagnose.Snapshot {
	return &diagnose.Snapshot{
		Target:      diagnose.Target{Kind: "pod", Namespace: "default"},
		Pods:        pods,
		Events:      map[string][]kubernetes.WarningEvent{},
		Logs:        map[string]map[string]kubernetes.ContainerLog{},
		CollectedAt: time.Now(),
	}
}

func findCategory(findings []diagnose.Finding, category string) *diagnose.Finding {
	for i := range findings {
		if findings[i].Category == category {
			return &findings[i]
		}
	}
	return nil
}

// assertEvidenceExists 校验每条发现引用的证据 ID 都真实存在且非空。
func assertEvidenceExists(t *testing.T, findings []diagnose.Finding, evidence []diagnose.Evidence) {
	t.Helper()
	ids := map[string]bool{}
	for _, e := range evidence {
		ids[e.ID] = true
	}
	for _, f := range findings {
		if len(f.EvidenceIDs) == 0 {
			t.Errorf("发现 %q 没有引用任何证据", f.Title)
		}
		for _, id := range f.EvidenceIDs {
			if !ids[id] {
				t.Errorf("发现 %q 引用了不存在的证据 %q", f.Title, id)
			}
		}
	}
}

func TestCrashLoopBackOff(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p1",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name:           "app",
			Ready:          false,
			Restarts:       5,
			Reason:         "CrashLoopBackOff",
			Image:          "registry/app:v1",
			LastTerminated: &kubernetes.Termination{Reason: "Error", ExitCode: 1},
		}},
	})
	snap.Logs["p1"] = map[string]kubernetes.ContainerLog{
		"app": {PodName: "p1", ContainerName: "app", TailLines: 100, Content: "panic: something went wrong\n"},
	}

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "crashloop")
	if f == nil {
		t.Fatalf("未识别出 CrashLoopBackOff，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityCritical {
		t.Errorf("期望严重程度为严重，实际 %s", f.Severity)
	}
	if f.Subject != "pod/p1 container/app" {
		t.Errorf("对象标识错误: %s", f.Subject)
	}
	// 命中 crashloop 后不应再被兜底规则重复报告。
	if findCategory(findings, "notready") != nil {
		t.Errorf("CrashLoop 的 Pod 不应再触发兜底规则")
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestImagePullBackOff(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p2",
		Phase: "Pending",
		Containers: []kubernetes.ContainerStatus{{
			Name:   "app",
			Reason: "ImagePullBackOff",
			Image:  "registry/app:v9",
		}},
	})
	snap.Events["p2"] = []kubernetes.WarningEvent{{
		Reason:  "Failed",
		Message: `Failed to pull image "registry/app:v9": rpc error`,
		Count:   3,
	}}

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "imagepull")
	if f == nil {
		t.Fatalf("未识别出 ImagePullBackOff，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityHigh {
		t.Errorf("期望严重程度为高，实际 %s", f.Severity)
	}
	if len(f.EvidenceIDs) < 2 {
		t.Errorf("期望同时引用状态与拉取事件证据，实际 %v", f.EvidenceIDs)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestOOMKilled(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p3",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name:           "app",
			Ready:          false,
			Restarts:       2,
			Image:          "registry/app:v1",
			MemoryLimit:    "256Mi",
			LastTerminated: &kubernetes.Termination{Reason: "OOMKilled", ExitCode: 137},
		}},
	})

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "oom")
	if f == nil {
		t.Fatalf("未识别出 OOMKilled，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityCritical {
		t.Errorf("期望严重程度为严重，实际 %s", f.Severity)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestPendingPod(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p4",
		Phase: "Pending",
		Conditions: []kubernetes.PodCondition{{
			Type:    "PodScheduled",
			Status:  "False",
			Reason:  "Unschedulable",
			Message: "0/3 nodes are available: 3 Insufficient memory.",
		}},
	})
	snap.Events["p4"] = []kubernetes.WarningEvent{{
		Reason:  "FailedScheduling",
		Message: "0/3 nodes are available: 3 Insufficient memory.",
	}}

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "pending")
	if f == nil {
		t.Fatalf("未识别出 Pending，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityHigh {
		t.Errorf("期望严重程度为高，实际 %s", f.Severity)
	}
	if got := f.Explanation; got != "Pod 已创建但没有节点能满足调度条件：集群资源不足" {
		t.Errorf("调度原因归类错误: %s", got)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestProbeFailure(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p5",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name:      "app",
			Ready:     false,
			Readiness: &kubernetes.ProbeInfo{Kind: "http-get", Target: ":8080/ready"},
		}},
	})
	snap.Events["p5"] = []kubernetes.WarningEvent{{
		Reason:  "Unhealthy",
		Message: "Readiness probe failed: HTTP probe failed with statuscode: 503",
	}}

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "probe")
	if f == nil {
		t.Fatalf("未识别出探针失败，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityMedium {
		t.Errorf("期望严重程度为中，实际 %s", f.Severity)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestDeploymentProgressDeadline(t *testing.T) {
	snap := newSnapshot()
	snap.Target = diagnose.Target{Kind: "deployment", Name: "web", Namespace: "default"}
	snap.Deployment = &kubernetes.DeploymentStatus{
		Name: "web",
		Conditions: []kubernetes.DeploymentCondition{{
			Type:    "Progressing",
			Status:  "False",
			Reason:  "ProgressDeadlineExceeded",
			Message: "Deployment has been progressing beyond the deadline",
		}},
	}
	snap.DeploymentEvents = []kubernetes.WarningEvent{{
		Reason:  "FailedCreate",
		Message: "exceeded quota",
	}}

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "deployment")
	if f == nil {
		t.Fatalf("未识别出 Deployment 发布失败，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityHigh {
		t.Errorf("期望严重程度为高，实际 %s", f.Severity)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestPodNotReadyCatchAll(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p6",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name:  "app",
			Ready: false, // 启动慢，没有具体异常 Reason 和事件
		}},
	})

	findings, evidence := diagnose.RunRules(snap)
	f := findCategory(findings, "notready")
	if f == nil {
		t.Fatalf("未就绪 Pod 应触发兜底规则，实际发现: %v", findings)
	}
	if f.Severity != diagnose.SeverityMedium {
		t.Errorf("期望严重程度为中，实际 %s", f.Severity)
	}
	assertEvidenceExists(t, findings, evidence)
}

func TestHealthySnapshotHasNoFindings(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p7",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name:  "app",
			Ready: true,
		}},
	})

	findings, _ := diagnose.RunRules(snap)
	if len(findings) != 0 {
		t.Errorf("健康快照不应有任何发现，实际: %v", findings)
	}
}
