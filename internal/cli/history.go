package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"opsagent/internal/observability"
	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// historyCmd 聚合历史记录子命令。
var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "查看和导出诊断历史",
}

// historyListCmd 以表格列出历史摘要。
var historyListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出诊断历史",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.list")
		defer span.End()

		repo, closeFn, err := openRepo(cmd)
		if err != nil {
			return err
		}
		defer closeFn()

		namespace := ""
		if cmd.Flags().Changed("namespace") {
			namespace, _ = cmd.Flags().GetString("namespace")
		}
		summaries, err := repo.List(ctx, namespace, 50)
		if err != nil {
			return err
		}
		renderHistoryList(cmd.OutOrStdout(), summaries)
		return nil
	},
}

// historyShowCmd 完整复现一条历史报告。
var historyShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "显示指定诊断报告",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.show")
		defer span.End()

		doc, closeFn, err := loadHistoryDocument(ctx, cmd, args[0])
		if err != nil {
			return err
		}
		defer closeFn()

		format, _ := cmd.Flags().GetString("format")
		f := normalizeFormat(format)
		renderer, err := report.For(f)
		if err != nil {
			return err
		}
		return renderer.Render(cmd.OutOrStdout(), doc)
	},
}

// historyExportCmd 把一条历史报告导出到文件。
var historyExportCmd = &cobra.Command{
	Use:   "export <id>",
	Short: "导出指定诊断报告",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, span := observability.Tracer().Start(cmd.Context(), "history.export")
		defer span.End()

		doc, closeFn, err := loadHistoryDocument(ctx, cmd, args[0])
		if err != nil {
			return err
		}
		defer closeFn()

		output, _ := cmd.Flags().GetString("output")
		if output == "" {
			return fmt.Errorf("请用 -o 指定导出文件")
		}
		format, _ := cmd.Flags().GetString("format")
		f, err := exportFormat(output, format)
		if err != nil {
			return err
		}
		return writeExport(output, f, doc)
	},
}

// openRepo 按 --history-db / OPSAGENT_HISTORY_DB / 默认路径打开仓储。
func openRepo(cmd *cobra.Command) (storage.Repository, func(), error) {
	path, _ := cmd.Flags().GetString("history-db")
	if path == "" {
		path = os.Getenv("OPSAGENT_HISTORY_DB")
	}
	if path == "" {
		path, _ = storage.DefaultPath()
	}
	db, err := storage.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开历史数据库失败: %w", err)
	}
	return db, func() { db.Close() }, nil
}

// openHistoryRepoBestEffort 尽力打开历史库；失败时返回 nil 仓储与一条 notice（不阻断诊断）。
func openHistoryRepoBestEffort(dbPath string) (storage.Repository, string) {
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Sprintf("历史数据库不可用（%v），本次不保存历史", err)
	}
	return db, ""
}

// loadHistoryDocument 读取一条记录并把 payload 反序列化为 Document。
func loadHistoryDocument(ctx context.Context, cmd *cobra.Command, idText string) (*report.Document, func(), error) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("历史记录 id 必须是数字，实际 %q", idText)
	}
	repo, closeFn, err := openRepo(cmd)
	if err != nil {
		return nil, nil, err
	}
	rec, err := repo.Get(ctx, id)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	var doc report.Document
	if err := json.Unmarshal(rec.Payload, &doc); err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("解析历史记录失败: %w", err)
	}
	return &doc, closeFn, nil
}

// renderHistoryList 用 tabwriter 渲染历史摘要表。
func renderHistoryList(out io.Writer, summaries []storage.RecordSummary) {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t时间\t命名空间\t对象\t发现数\t最高严重度")
	for _, s := range summaries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\n",
			s.ID, s.CreatedAt.Format("2006-01-02 15:04:05"), s.Namespace,
			s.TargetDisplay, s.FindingsCount, s.HighestSeverity)
	}
	w.Flush()
}

// normalizeFormat 统一格式别名；空值默认 markdown。
func normalizeFormat(format string) string {
	switch format {
	case "md":
		return "markdown"
	case "", "markdown", "html":
		if format == "" {
			return "markdown"
		}
		return format
	default:
		return format
	}
}

func init() {
	historyListCmd.Flags().String("format", "", "输出格式: markdown、html")
	historyShowCmd.Flags().String("format", "", "输出格式: markdown、html")
	historyExportCmd.Flags().StringP("output", "o", "", "导出文件路径")
	historyExportCmd.Flags().String("format", "", "强制指定格式: markdown、html")
	historyCmd.AddCommand(historyListCmd, historyShowCmd, historyExportCmd)
	rootCmd.AddCommand(historyCmd)
}
