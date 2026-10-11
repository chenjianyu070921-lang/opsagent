package diagnose_test

import (
	"testing"
	"time"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
)

func TestEvidenceAddWith(t *testing.T) {
	set := diagnose.NewEvidenceSet()
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	id := set.AddWith(diagnose.Evidence{
		Kind:      "metric",
		Source:    "pod/p container/app",
		Summary:   "趋势",
		Timestamp: &ts,
		Links:     []diagnose.EvidenceLink{{Type: "graph", Label: "图表", URL: "http://prom/graph"}},
	})
	if id != "ev-1" {
		t.Errorf("第一个证据编号应为 ev-1，实际 %s", id)
	}
	id2 := set.Add("status", "pod/p", "状态", "细节")
	if id2 != "ev-2" {
		t.Errorf("Add 编号应为 ev-2，实际 %s", id2)
	}
	got := set.List()
	if len(got) != 2 || got[0].Timestamp == nil || len(got[0].Links) != 1 {
		t.Errorf("证据元数据未保留: %+v", got)
	}
}

func TestEventEvidenceTimestampFromEvent(t *testing.T) {
	eventTime := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)
	snap := newSnapshot(kubernetes.PodStatus{
		Name:  "p8",
		Phase: "Running",
		Containers: []kubernetes.ContainerStatus{{
			Name: "app", Ready: false,
		}},
	})
	snap.Events["p8"] = []kubernetes.WarningEvent{{
		Reason: "FailedScheduling", Message: "no eligible node", LastTimestamp: eventTime,
	}}

	findings, evidence := diagnose.RunRules(snap)
	assertEvidenceExists(t, findings, evidence)

	var eventEv *diagnose.Evidence
	for i := range evidence {
		if evidence[i].Kind == "event" {
			eventEv = &evidence[i]
		}
	}
	if eventEv == nil {
		t.Fatalf("未找到事件证据")
	}
	if eventEv.Timestamp == nil || !eventEv.Timestamp.Equal(eventTime) {
		t.Errorf("事件证据时间戳应为事件 LastTimestamp，实际 %v", eventEv.Timestamp)
	}

	// 其余证据也必须有时间戳。
	for _, e := range evidence {
		if e.Timestamp == nil {
			t.Errorf("证据 %s 缺少时间戳", e.ID)
		}
	}
}
