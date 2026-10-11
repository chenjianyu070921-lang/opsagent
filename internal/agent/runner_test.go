package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"opsagent/internal/agent"
	"opsagent/internal/diagnose"
	"opsagent/internal/prometheus"
	"opsagent/internal/storage"
)

// fakeQuerier 是可切换报错的指标查询假实现。
type fakeQuerier struct {
	err error
}

func (f *fakeQuerier) Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*prometheus.ContainerMetrics, error) {
	return nil, f.err
}

// fakeRepo 是可切换保存报错的仓储假实现。
type fakeRepo struct {
	saveErr error
}

func (f *fakeRepo) Save(ctx context.Context, r storage.Record) (int64, error) {
	if f.saveErr != nil {
		return 0, f.saveErr
	}
	return 42, nil
}
func (f *fakeRepo) List(ctx context.Context, namespace string, limit int) ([]storage.RecordSummary, error) {
	return nil, nil
}
func (f *fakeRepo) Get(ctx context.Context, id int64) (*storage.Record, error) {
	return nil, storage.ErrNotFound
}
func (f *fakeRepo) Latest(ctx context.Context) (*storage.Record, error) {
	return nil, storage.ErrNotFound
}

// healthyReadyPod 构造一个运行正常、不会触发日志采集的 Pod。
func healthyReadyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name)},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:v1"}},
		},
	}
}

func runWith(t *testing.T, deps agent.Deps) *agent.Output {
	t.Helper()
	r := agent.New(deps)
	out, err := r.Run(context.Background(), agent.Input{
		Target:        diagnose.Target{Kind: "pod", Name: "p1", Namespace: "default"},
		Since:         30 * time.Minute,
		Tail:          100,
		MetricsWindow: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	return out
}

func TestRunHappyPath(t *testing.T) {
	var k8s kubernetes.Interface = fake.NewSimpleClientset(healthyReadyPod("p1"))
	out := runWith(t, agent.Deps{K8s: k8s})
	if out.Document == nil {
		t.Fatalf("应产出 Document")
	}
	if out.RecordID != 0 {
		t.Errorf("未提供 Repo 时不应落库，实际 %d", out.RecordID)
	}
}

func TestRunMetricsFailureProducesNotice(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	deps := agent.Deps{
		K8s:     k8s,
		Prom:    &fakeQuerier{err: errors.New("prometheus 超时")},
		PromURL: "http://prom:9090",
	}
	out := runWith(t, deps)
	if out.Document == nil {
		t.Fatalf("指标失败不应阻断报告")
	}
	found := false
	for _, n := range out.Notices {
		if stringsContains(n, "prometheus 超时") {
			found = true
		}
	}
	if !found {
		t.Errorf("应在 Notices 中包含指标降级原因，实际 %v", out.Notices)
	}
}

func TestRunPersistFailureProducesNotice(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	deps := agent.Deps{
		K8s:  k8s,
		Repo: &fakeRepo{saveErr: errors.New("disk full")},
	}
	out := runWith(t, deps)
	if out.Document == nil {
		t.Fatalf("落库失败不应阻断 Document")
	}
	if out.RecordID != 0 {
		t.Errorf("保存失败时 RecordID 应为 0，实际 %d", out.RecordID)
	}
	found := false
	for _, n := range out.Notices {
		if stringsContains(n, "disk full") {
			found = true
		}
	}
	if !found {
		t.Errorf("应在 Notices 中包含落库失败原因，实际 %v", out.Notices)
	}
}

func TestRunPersistSuccessSetsRecordID(t *testing.T) {
	k8s := fake.NewSimpleClientset(healthyReadyPod("p1"))
	out := runWith(t, agent.Deps{K8s: k8s, Repo: &fakeRepo{}})
	if out.RecordID != 42 {
		t.Errorf("落库成功应返回记录 id，实际 %d", out.RecordID)
	}
}

func TestRunCollectFailureHardFails(t *testing.T) {
	// 空 fake clientset，Get 不存在的 Pod 应硬失败。
	r := agent.New(agent.Deps{K8s: fake.NewSimpleClientset()})
	_, err := r.Run(context.Background(), agent.Input{
		Target: diagnose.Target{Kind: "pod", Name: "missing", Namespace: "default"},
	})
	if err == nil {
		t.Errorf("采集失败应返回错误")
	}
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
