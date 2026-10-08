// Package kubernetes 封装与 Kubernetes API 的交互，负责采集诊断所需的集群证据。
package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// NewClient 创建 Kubernetes clientset（访问各类资源的总入口）。
// kubeconfigPath 为空时按顺序尝试：KUBECONFIG 环境变量 → ~/.kube/config → 集群内 ServiceAccount。
func NewClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	config, err := newRestConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("创建 Kubernetes 客户端失败: %w", err)
	}
	return clientset, nil
}

// newRestConfig 决定"连哪个集群、用什么凭证"。
func newRestConfig(kubeconfigPath string) (*rest.Config, error) {
	// 1) 显式参数 → 2) KUBECONFIG 环境变量 → 3) 默认的 ~/.kube/config
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("KUBECONFIG")
	}
	if kubeconfigPath == "" {
		if home := homedir.HomeDir(); home != "" {
			candidate := filepath.Join(home, ".kube", "config")
			if _, err := os.Stat(candidate); err == nil {
				kubeconfigPath = candidate
			}
		}
	}

	// 集群外运行：从 kubeconfig 文件加载（与 kubectl 同源）
	if kubeconfigPath != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("加载 kubeconfig %s 失败: %w", kubeconfigPath, err)
		}
		return config, nil
	}

	// 集群内运行：使用 Pod 自动挂载的 ServiceAccount 凭证
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("找不到集群凭证：请用 --kubeconfig 指定 kubeconfig，或在集群内以 ServiceAccount 运行")
	}
	return config, nil
}
