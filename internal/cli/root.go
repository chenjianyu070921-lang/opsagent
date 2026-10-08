// Package cli 定义 opsagent 的命令行入口，所有子命令都挂在根命令上。
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// rootCmd 是整个 CLI 的根命令。
var rootCmd = &cobra.Command{
	Use:   "opsagent",
	Short: "Kubernetes 运维诊断工具",
	Long:  "opsagent 是一个只读的 Kubernetes 故障诊断工具，采集集群证据并生成分析报告。",
}

// Execute 由 main 调用，执行命令行解析并运行匹配到的子命令。
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	// 根命令级别的全局 flag，所有子命令都能访问。
	rootCmd.PersistentFlags().StringP("namespace", "n", "default", "Kubernetes 命名空间")
	rootCmd.PersistentFlags().String("kubeconfig", "", "kubeconfig 文件路径（默认用 KUBECONFIG 环境变量或 ~/.kube/config）")
}
