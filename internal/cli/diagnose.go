package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"opsagent/internal/diagnose"
	"opsagent/internal/kubernetes"
	"opsagent/internal/llm"
	"opsagent/internal/report"
)

// diagnoseCmd 对应 `opsagent diagnose`，负责解析参数并编排证据采集、规则分析与报告输出。
var diagnoseCmd = &cobra.Command{
	Use:   "diagnose [资源类型/资源名 | 资源类型 资源名]",
	Short: "诊断指定的 Kubernetes 资源",
	Long:  "采集指定资源（pod/deployment）的状态、事件、日志等证据，由规则引擎输出带证据链的诊断报告。",
	Args:  cobra.MaximumNArgs(2),
	// 用 RunE 而不是 Run：业务逻辑出错时把 error 返回给 Cobra，
	// 由它统一打印错误信息并以非零状态码退出。
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, _ := cmd.Flags().GetString("namespace")
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		since, _ := cmd.Flags().GetDuration("since")
		tail, _ := cmd.Flags().GetInt64("tail")
		output, _ := cmd.Flags().GetString("output")
		useLLM, _ := cmd.Flags().GetBool("llm")
		modelOverride, _ := cmd.Flags().GetString("llm-model")

		client, err := kubernetes.NewClient(kubeconfig)
		if err != nil {
			return err
		}

		target := parseTarget(args)
		target.Namespace = namespace

		snap, err := diagnose.Collect(cmd.Context(), client, target, since, tail)
		if err != nil {
			return err
		}

		findings, evidence := diagnose.RunRules(snap)

		// LLM 分析默认关闭；开启时即使调用失败也不影响规则报告，只在 stderr 提示。
		var analysis *llm.Analysis
		if useLLM {
			cfg, err := llm.LoadConfig()
			if err != nil {
				return err
			}
			if modelOverride != "" {
				cfg.Model = modelOverride
			}
			analysis, err = llm.Analyze(cmd.Context(), cfg, snap, findings, evidence)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "警告: %v，本次仅输出规则分析\n\n", err)
				analysis = nil
			}
		}

		switch output {
		case "markdown":
			fmt.Fprintln(cmd.OutOrStdout(), report.RenderMarkdown(snap, findings, evidence, analysis))
		case "table":
			renderTable(cmd.OutOrStdout(), snap)
		default:
			return fmt.Errorf("未知输出格式 %q，支持: markdown、table", output)
		}
		return nil
	},
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
