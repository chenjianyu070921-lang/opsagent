// Package prometheus 封装 Prometheus HTTP API：指标领域类型、固定查询、地址解析与证据链接。
// 本包是叶子包，不依赖任何 opsagent 内部包。
package prometheus

import (
	"context"
	"time"
)

// Point 是一个指标采样点。
type Point struct {
	T time.Time
	V float64
}

// ContainerMetrics 是单个容器在一个时间窗口内的内存与 CPU 指标。
// 序列为空表示该类指标未取到；Limit 为 nil 表示集群缺少 limit 指标。
type ContainerMetrics struct {
	Pod       string
	Container string
	// 内存工作集（cAdvisor working_set）与其 limit，以及窗口内峰值占比。
	MemoryWorkingSet []Point
	MemoryLimit      *float64
	MemoryPeakRatio  float64
	// CPU 使用量（核）、CPU limit、限流时间片比率序列。
	CPUUsageCores     []Point
	CPULimit          *float64
	CPUThrottledRatio []Point
}

// Querier 是指标查询的边界接口，真实实现与假实现对等可换。
type Querier interface {
	// Query 按命名空间和 Pod 名集合查询一个时间窗口的容器指标。
	// podNames 为空时不加 Pod 过滤。
	Query(ctx context.Context, namespace string, podNames []string, window time.Duration) (map[string]map[string]*ContainerMetrics, error)
}
