package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"opsagent/internal/agent"
	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/observability"
	"opsagent/internal/prometheus"
	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// diagnoseCmd 对应 `opsagent diagnose`，只负责参数解析、依赖装配与最终渲染输出。
var diagnoseCmd = &cobra.Command{
	Use:   "diagnose [资源类型/资源名 | 资源类型 资源名]",
	Short: "诊断指定的 Kubernetes 资源",
	Long:  "采集指定资源（pod/deployment）的状态、事件、日志、指标等证据，由规则引擎输出带证据链的诊断报告，并保存为历史记录。",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, _ := cmd.Flags().GetString("namespace")
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		since, _ := cmd.Flags().GetDuration("since")
		tail, _ := cmd.Flags().GetInt64("tail")
		output, _ := cmd.Flags().GetString("output")
		useLLM, _ := cmd.Flags().GetBool("llm")
		modelOverride, _ := cmd.Flags().GetString("llm-model")
		promURLFlag, _ := cmd.Flags().GetString("prometheus-url")
		grafanaURL, _ := cmd.Flags().GetString("grafana-url")
		if grafanaURL == "" {
			grafanaURL = os.Getenv("OPSAGENT_GRAFANA_URL")
		}
		metricsWindow, _ := cmd.Flags().GetDuration("metrics-window")
		exportPath, _ := cmd.Flags().GetString("export")
		formatFlag, _ := cmd.Flags().GetString("format")
		last, _ := cmd.Flags().GetBool("last")

		if err := validateLastArgs(last, args); err != nil {
			return err
		}
		if last {
			return runLast(cmd, namespace, output, exportPath, formatFlag)
		}

		// 导出格式在采集前确定，避免采完才发现格式不支持。
		var exportFormatName string
		if exportPath != "" {
			f, err := exportFormat(exportPath, formatFlag)
			if err != nil {
				return err
			}
			exportFormatName = f
		}
		if output == "table" && exportPath != "" {
			return fmt.Errorf("表格格式不支持导出，请改用 markdown 或 html")
		}

		client, err := kubernetes.NewClient(kubeconfig)
		if err != nil {
			return err
		}

		// Prometheus 地址解析；不可达时 Prom 置 nil，管线降级为纯规则。
		var promQuerier prometheus.Querier
		var resolvedURL string
		var preNotices []string
		baseURL, _, err := prometheus.ResolveURL(cmd.Context(), promURLFlag, client)
		if err == nil {
			querier, qerr := prometheus.New(prometheus.Config{BaseURL: baseURL})
			if qerr != nil {
				preNotices = append(preNotices, fmt.Sprintf("Prometheus 客户端创建失败：%v，跳过指标分析", qerr))
			} else {
				promQuerier = querier
				resolvedURL = baseURL
			}
		}

		// 历史库 best-effort：打不开时告警进 notice 区，不阻断诊断。
		var repo storage.Repository
		dbPath, _ := cmd.Flags().GetString("history-db")
		if dbPath == "" {
			dbPath = os.Getenv("OPSAGENT_HISTORY_DB")
		}
		if dbPath == "" {
			dbPath, _ = storage.DefaultPath()
		}
		repo, notice := openHistoryRepoBestEffort(dbPath)
		if repo == nil && notice != "" {
			preNotices = append(preNotices, notice)
		}

		target := parseTarget(args)
		target.Namespace = namespace

		r := agent.New(agent.Deps{
			K8s:     client,
			Prom:    promQuerier,
			PromURL: resolvedURL,
			Repo:    repo,
		})
		out, err := r.Run(cmd.Context(), agent.Input{
			Target:        target,
			Since:         since,
			Tail:          tail,
			UseLLM:        useLLM,
			ModelOverride: modelOverride,
			MetricsWindow: metricsWindow,
			GrafanaURL:    grafanaURL,
			Notices:       preNotices,
		})
		if err != nil {
			return err
		}

		// 降级提示同时输出到 stderr（报告正文 Notices 区也会展示）。
		for _, n := range out.Document.Notices {
			fmt.Fprintf(cmd.ErrOrStderr(), "提示: %s\n", n)
		}

		if exportPath != "" {
			if err := writeExport(exportPath, exportFormatName, out.Document); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "已导出报告到 %s（历史记录 #%d）\n", exportPath, out.RecordID)
			return nil
		}

		switch output {
		case "markdown":
			renderer, err := report.For("markdown")
			if err != nil {
				return err
			}
			return renderer.Render(cmd.OutOrStdout(), out.Document)
		case "table":
			renderTable(cmd.OutOrStdout(), out.Snapshot)
			return nil
		default:
			return fmt.Errorf("未知输出格式 %q，支持: markdown、table", output)
		}
	},
}

// validateLastArgs 校验 --last 不能与诊断目标同时给出。
func validateLastArgs(last bool, args []string) error {
	if last && len(args) > 0 {
		return fmt.Errorf("--last 用于查看最近一次诊断，不能同时指定诊断目标")
	}
	return nil
}

// exportFormat 确定导出格式：explicit 优先，否则按扩展名推断；无法推断时报错。
func exportFormat(path, explicit string) (string, error) {
	if explicit != "" {
		switch explicit {
		case "markdown", "md", "html":
			if explicit == "md" {
				return "markdown", nil
			}
			return explicit, nil
		default:
			return "", fmt.Errorf("不支持的格式 %q，支持: markdown、html", explicit)
		}
	}
	if f := report.FormatFromPath(path); f != "" {
		return f, nil
	}
	return "", fmt.Errorf("无法从文件名 %q 推断格式，请用 --format 指定（支持: markdown、html）", path)
}

// writeExport 用指定格式渲染 Document 并写入文件。
func writeExport(path, format string, doc *report.Document) error {
	renderer, err := report.For(format)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建导出文件 %s 失败: %w", path, err)
	}
	defer f.Close()
	if err := renderer.Render(f, doc); err != nil {
		return fmt.Errorf("写入导出文件失败: %w", err)
	}
	return nil
}

// runLast 处理 --last：不触碰集群，取最近记录渲染（可选导出）。
func runLast(cmd *cobra.Command, namespace, output, exportPath, formatFlag string) error {
	ctx, span := observability.Tracer().Start(cmd.Context(), "history.latest")
	defer span.End()

	dbPath, _ := cmd.Flags().GetString("history-db")
	if dbPath == "" {
		dbPath = os.Getenv("OPSAGENT_HISTORY_DB")
	}
	if dbPath == "" {
		dbPath, _ = storage.DefaultPath()
	}
	repo, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("打开历史数据库失败: %w", err)
	}
	defer repo.Close()
	rec, err := repo.Latest(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("暂无诊断历史")
		}
		return err
	}

	var doc report.Document
	if uerr := json.Unmarshal(rec.Payload, &doc); uerr != nil {
		return fmt.Errorf("解析历史记录失败: %w", uerr)
	}

	if exportPath != "" {
		f, ferr := exportFormat(exportPath, formatFlag)
		if ferr != nil {
			return ferr
		}
		if werr := writeExport(exportPath, f, &doc); werr != nil {
			return werr
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "已导出报告到 %s（历史记录 #%d）\n", exportPath, rec.ID)
		return nil
	}

	f := "markdown"
	if formatFlag != "" {
		if formatFlag == "md" {
			f = "markdown"
		} else {
			f = formatFlag
		}
	}
	if output == "table" {
		return fmt.Errorf("--last 不支持表格格式")
	}
	renderer, err := report.For(f)
	if err != nil {
		return err
	}
	return renderer.Render(cmd.OutOrStdout(), &doc)
}

// parseTarget 把命令行参数解析成诊断目标，兼容三种写法：
// diagnose（全部 Pod）、diagnose pod/name（斜杠形式）、diagnose pod name（两参数形式）。
func parseTarget(args []string) diagnose.Target {
	if len(args) == 0 {
		return diagnose.Target{Kind: "all"}
	}

	kind, name := args[0], ""
	if strings.Contains(kind, "/") {
		parts := strings.SplitN(kind, "/", 2)
		kind, name = parts[0], parts[1]
	}
	if len(args) == 2 {
		kind, name = args[0], args[1]
	}
	return diagnose.Target{Kind: normalizeKind(kind), Name: name}
}

// normalizeKind 统一资源类型的各种别名；"all" 和无法识别的裸词视为全部 Pod。
func normalizeKind(kind string) string {
	switch kind {
	case "pod", "pods", "po":
		return "pod"
	case "deployment", "deployments", "deploy":
		return "deployment"
	default:
		return "all"
	}
}

func init() {
	diagnoseCmd.Flags().Duration("since", 30*time.Minute, "日志采集的时间窗口")
	diagnoseCmd.Flags().Int64("tail", 100, "每个容器最多取的日志行数")
	diagnoseCmd.Flags().StringP("output", "o", "markdown", "输出格式: markdown、table")
	diagnoseCmd.Flags().Bool("llm", false, "启用 LLM 根因分析（需配置 OPSAGENT_LLM_* 环境变量）")
	diagnoseCmd.Flags().String("llm-model", "", "覆盖 OPSAGENT_LLM_MODEL 指定的模型")
	diagnoseCmd.Flags().String("prometheus-url", "", "Prometheus 地址（默认取 OPSAGENT_PROMETHEUS_URL 或集群内自动探测）")
	diagnoseCmd.Flags().String("grafana-url", "", "Grafana 地址，用于生成 Explore 链接（或用 OPSAGENT_GRAFANA_URL）")
	diagnoseCmd.Flags().Duration("metrics-window", 30*time.Minute, "Prometheus 指标查询窗口")
	diagnoseCmd.Flags().String("export", "", "把报告导出到指定文件（格式按扩展名或 --format）")
	diagnoseCmd.Flags().String("format", "", "强制指定输出/导出格式: markdown、html")
	diagnoseCmd.Flags().Bool("last", false, "查看最近一次诊断（不采集集群）")
}

// renderTable 以表格形式输出 Pod 摘要，并附上各 Pod 的 Warning 事件。
func renderTable(out io.Writer, snap *diagnose.Snapshot) {
	if d := snap.Deployment; d != nil {
		fmt.Fprintf(out, "Deployment: %s (desired=%d ready=%d available=%d)\n\n",
			d.Name, d.Replicas.Desired, d.Replicas.Ready, d.Replicas.Available)
	}

	if len(snap.Pods) == 0 {
		fmt.Fprintf(out, "命名空间 %q 中没有 Pod\n", snap.Target.Namespace)
		return
	}

	fmt.Fprintf(out, "命名空间: %s\n\n", snap.Target.Namespace)

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "POD\tPHASE\tREADY\tRESTARTS\tSTATUS")
	for _, pod := range snap.Pods {
		ready := 0
		restarts := int32(0)
		reasons := make([]string, 0)
		for _, c := range pod.Containers {
			if c.Ready {
				ready++
			}
			restarts += c.Restarts
			if c.Reason != "" {
				reasons = append(reasons, fmt.Sprintf("%s: %s", c.Name, c.Reason))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d\t%s\n",
			pod.Name, pod.Phase, ready, len(pod.Containers), restarts, strings.Join(reasons, ", "))
	}
	w.Flush()

	for _, pod := range snap.Pods {
		if len(snap.Events[pod.Name]) > 0 {
			fmt.Fprintf(out, "\n%s 的 Warning 事件:\n", pod.Name)
		}
		for _, e := range snap.Events[pod.Name] {
			fmt.Fprintf(out, "  - [%s] %s (x%d)\n", e.Reason, e.Message, e.Count)
		}
	}
}
