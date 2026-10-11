// Package cli 定义 opsagent 的命令行入口，所有子命令都挂在根命令上。
package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// rootCmd 是整个 CLI 的根命令。
var rootCmd = &cobra.Command{
	Use:   "opsagent",
	Short: "Kubernetes 运维诊断工具",
	Long:  "opsagent 是一个只读的 Kubernetes 故障诊断工具，采集集群证据并生成分析报告。",
}

// Execute 把外部 context 注入根命令并执行。
func Execute(ctx context.Context) error {
	rootCmd.SetContext(ctx)
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringP("namespace", "n", "default", "Kubernetes 命名空间")
	rootCmd.PersistentFlags().String("kubeconfig", "", "kubeconfig 文件路径（默认用 KUBECONFIG 环境变量或 ~/.kube/config）")
	rootCmd.PersistentFlags().String("history-db", "", "SQLite 历史数据库路径（默认用户配置目录下 opsagent/history.db，或用 OPSAGENT_HISTORY_DB）")
}
