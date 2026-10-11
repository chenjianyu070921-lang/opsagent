// Package agent 实现固定阶段的诊断管线，是 CLI/TUI/MCP 复用的统一入口。
// 纪律：只允许 Runner 一个固定管线，不引入 Plan/Step/工具注册表。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	k8sclient "k8s.io/client-go/kubernetes"

	"opsagent/internal/diagnose"
	"opsagent/internal/llm"
	"opsagent/internal/observability"
	"opsagent/internal/prometheus"
	"opsagent/internal/report"
	"opsagent/internal/storage"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Deps 是管线的外部依赖，全部显式注入；Prom/Repo 可为 nil。
type Deps struct {
	K8s     k8sclient.Interface
	Prom    prometheus.Querier
	PromURL string
	Repo    storage.Repository
}

// Input 是一次诊断请求的参数。
type Input struct {
	Target        diagnose.Target
	Since         time.Duration
	Tail          int64
	UseLLM        bool
	ModelOverride string
	MetricsWindow time.Duration
	GrafanaURL    string
	Notices       []string // 装配阶段已产生的降级提示
}

// Output 是管线产出：Document 供渲染，Snapshot 供表格视图。
type Output struct {
	Document *report.Document
	Snapshot *diagnose.Snapshot
	RecordID int64
	Notices  []string
}

// Runner 执行固定阶段诊断管线。
type Runner struct {
	deps Deps
}

// New 创建 Runner。
func New(deps Deps) *Runner {
	return &Runner{deps: deps}
}

// Run 依次执行 collect→metrics→rules→llm→build→persist。
func (r *Runner) Run(ctx context.Context, in Input) (*Output, error) {
	ctx, rootSpan := observability.Tracer().Start(ctx, observability.SpanDiagnose)
	defer rootSpan.End()

	startedAt := time.Now()
	notices := append([]string{}, in.Notices...)
	addNotice := func(format string, args ...interface{}) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}
	tracer := observability.Tracer()

	// 1) collect：硬失败。
	var snap *diagnose.Snapshot
	if err := stage(ctx, tracer, observability.SpanCollect, func(ctx context.Context) error {
		var err error
		snap, err = diagnose.Collect(ctx, r.deps.K8s, in.Target, in.Since, in.Tail)
		return err
	}); err != nil {
		rootSpan.RecordError(err)
		return nil, err
	}

	// 2) metrics：失败降级。
	if err := stage(ctx, tracer, observability.SpanPrometheus, func(ctx context.Context) error {
		snap.PrometheusURL = r.deps.PromURL
		snap.MetricsWindow = in.MetricsWindow
		snap.GrafanaURL = in.GrafanaURL
		if r.deps.Prom == nil {
			return fmt.Errorf("Prometheus 未配置或不可达，跳过指标分析")
		}
		podNames := make([]string, 0, len(snap.Pods))
		for _, p := range snap.Pods {
			podNames = append(podNames, p.Name)
		}
		metrics, err := r.deps.Prom.Query(ctx, snap.Target.Namespace, podNames, in.MetricsWindow)
		if err != nil {
			return err
		}
		snap.Metrics = metrics
		if len(metrics) == 0 {
			return fmt.Errorf("Prometheus 返回空指标")
		}
		return nil
	}); err != nil {
		addNotice("指标阶段：%v", err)
	}

	// 3) rules。
	var findings []diagnose.Finding
	var evidence []diagnose.Evidence
	stage(ctx, tracer, observability.SpanRules, func(ctx context.Context) error {
		findings, evidence = diagnose.RunRules(snap)
		return nil
	})

	// 4) llm：失败降级。
	var analysis *llm.Analysis
	if in.UseLLM {
		if err := stage(ctx, tracer, observability.SpanLLM, func(ctx context.Context) error {
			cfg, err := llm.LoadConfig()
			if err != nil {
				return err
			}
			if in.ModelOverride != "" {
				cfg.Model = in.ModelOverride
			}
			analysis, err = llm.Analyze(ctx, cfg, snap, findings, evidence)
			return err
		}); err != nil {
			addNotice("LLM 分析失败：%v，本次仅输出规则分析", err)
			analysis = nil
		}
	}

	// 5) build Document。
	var doc *report.Document
	stage(ctx, tracer, observability.SpanBuild, func(ctx context.Context) error {
		doc = report.BuildDocument(snap, findings, evidence, analysis, notices)
		return nil
	})

	// 6) persist：失败降级。
	var recordID int64
	if r.deps.Repo != nil {
		if err := stage(ctx, tracer, observability.SpanPersist, func(ctx context.Context) error {
			id, err := r.saveRecord(ctx, doc, snap, startedAt, analysis)
			if err != nil {
				return err
			}
			recordID = id
			return nil
		}); err != nil {
			// Document 已构建，需把降级提示同时追加进 doc.Notices，保证渲染可见。
			msg := fmt.Sprintf("历史记录保存失败：%v", err)
			notices = append(notices, msg)
			doc.Notices = append(doc.Notices, msg)
			recordID = 0
		}
	}

	return &Output{
		Document: doc,
		Snapshot: snap,
		RecordID: recordID,
		Notices:  notices,
	}, nil
}

// saveRecord 把 Document 序列化为 payload 并写入仓储。
func (r *Runner) saveRecord(ctx context.Context, doc *report.Document, snap *diagnose.Snapshot, startedAt time.Time, analysis *llm.Analysis) (int64, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}

	llmModel := ""
	if analysis != nil {
		llmModel = analysis.Model
	}
	targetKind := snap.Target.Kind
	if targetKind == "" {
		targetKind = "all"
	}

	return r.deps.Repo.Save(ctx, storage.Record{
		CreatedAt:       doc.GeneratedAt,
		Namespace:       doc.Namespace,
		TargetKind:      targetKind,
		TargetName:      snap.Target.Name,
		TargetDisplay:   doc.TargetDisplay,
		FindingsCount:   len(doc.Findings),
		HighestSeverity: highestSeverity(doc.Findings),
		DurationMS:      time.Since(startedAt).Milliseconds(),
		PrometheusURL:   snap.PrometheusURL,
		LLMModel:        llmModel,
		SchemaVersion:   1,
		Payload:         payload,
	})
}

// highestSeverity 在未排序的发现中取最高严重度；无发现时返回空串。
func highestSeverity(findings []report.DocFinding) string {
	highest := ""
	best := 99
	for _, f := range findings {
		if rank := docSeverityRank(f.Severity); rank < best {
			best = rank
			highest = f.Severity
		}
	}
	return highest
}

// docSeverityRank 与 diagnose 规则的严重度顺序一致，未知严重度排最后。
func docSeverityRank(severity string) int {
	switch severity {
	case "严重":
		return 0
	case "高":
		return 1
	case "中":
		return 2
	case "低":
		return 3
	default:
		return 4
	}
}

// stage 执行一个命名阶段并打 span；返回阶段错误供调用方决定降级。
func stage(ctx context.Context, tracer trace.Tracer, name string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, name)
	defer span.End()
	if err := fn(ctx); err != nil {
		span.SetAttributes(attribute.Bool("opsagent.ok", false))
		span.AddEvent("降级: " + err.Error())
		return err
	}
	return nil
}
