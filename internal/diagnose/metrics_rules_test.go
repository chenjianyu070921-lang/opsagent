package diagnose_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/prometheus"
)

func metricsSnapshot(podName string, limit float64, peak float64, throttle float64) *diagnose.Snapshot {
	now := time.Now()
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  podName,
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name: "app", Ready: true,
		}},
	})
	snap.PrometheusURL = "http://prom:9090"
	snap.MetricsWindow = 30 * time.Minute
	m := &prometheus.ContainerMetrics{
		Pod:       podName,
		Container: "app",
		MemoryWorkingSet: []prometheus.Point{
			{T: now.Add(-time.Minute), V: peak},
		},
		MemoryLimit: &limit,
		CPUThrottledRatio: []prometheus.Point{
			{T: now.Add(-time.Minute), V: throttle},
		},
	}
	snap.Metrics = map[string]map[string]*prometheus.ContainerMetrics{
		podName: {"app": m},
	}
	return snap
}

func TestMemoryTrendRuleThreshold(t *testing.T) {
	// 峰值 85%（含）时报内存压力；84% 不报。
	snap := metricsSnapshot("p85", 100, 85, 0)
	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)
	f := findCategory(findings, "memory-pressure")
	if f == nil {
		t.Fatalf("峰值 85%% 应报内存压力，实际 %v", findings)
	}
	if f.Severity != diagnose.SeverityMedium {
		t.Errorf("内存压力严重度应为中，实际 %s", f.Severity)
	}

	snap = metricsSnapshot("p84", 100, 84, 0)
	findings, _ = diagnose.RunRules(snap)
	if findCategory(findings, "memory-pressure") != nil {
		t.Errorf("峰值 84%% 不应报内存压力: %v", findings)
	}
}

func TestMemoryTrendSuppressedByOOM(t *testing.T) {
	snap := metricsSnapshot("pOOM", 100, 95, 0)
	// 追加 OOMKilled 状态。
	snap.Pods[0].Containers[0].Ready = false
	snap.Pods[0].Containers[0].LastTerminated = &kubernetes.Termination{
		Reason: "OOMKilled", ExitCode: 137,
	}
	findings, _ := diagnose.RunRules(snap)
	if findCategory(findings, "memory-pressure") != nil {
		t.Errorf("已有 OOM 发现时不应重复报内存压力: %v", findings)
	}
}

func TestCPUThrottleRuleThreshold(t *testing.T) {
	// 限流比率均值 25%（含）时报；24% 不报。
	snap := metricsSnapshot("pThr", 100, 0, 0.25)
	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)
	f := findCategory(findings, "cpu-throttle")
	if f == nil {
		t.Fatalf("限流 25%% 应报 CPU 限流，实际 %v", findings)
	}
	if f.Severity != diagnose.SeverityLow {
		t.Errorf("CPU 限流严重度应为低，实际 %s", f.Severity)
	}

	snap = metricsSnapshot("pOk", 100, 0, 0.24)
	findings, _ = diagnose.RunRules(snap)
	if findCategory(findings, "cpu-throttle") != nil {
		t.Errorf("限流 24%% 不应报 CPU 限流: %v", findings)
	}
}

func TestOOMRuleIncludesMetricEvidence(t *testing.T) {
	snap := metricsSnapshot("pOOM2", 100, 95, 0)
	snap.Pods[0].Containers[0].Ready = false
	snap.Pods[0].Containers[0].LastTerminated = &kubernetes.Termination{
		Reason: "OOMKilled", ExitCode: 137,
	}
	_, evidence := diagnose.RunRules(snap)
	found := false
	for _, e := range evidence {
		if e.Kind == "metric" {
			found = true
			if len(e.Links) == 0 {
				t.Errorf("指标证据应携带 graph 链接")
			}
		}
	}
	if !found {
		t.Errorf("OOM 诊断应附带指标证据，实际证据: %+v", evidence)
	}
}

func TestMetricsAbsentBehavesLikePhase1(t *testing.T) {
	snap := newSnapshot(kubernetes.PodStatus{
		Name: "pPlain", Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{Name: "app", Ready: true}},
	})
	findings, _ := diagnose.RunRules(snap)
	if len(findings) != 0 {
		t.Errorf("无指标且健康时不应有发现: %v", findings)
	}
}
