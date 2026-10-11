package prometheus

import (
	"fmt"
	"regexp"
	"strings"
)

// podMatcher 把 Pod 名集合转成锚定的正则 label 选择器；名字经 QuoteMeta 转义，
// 防止名称里的特殊字符注入 PromQL。空集合返回空串。
func podMatcher(podNames []string) string {
	if len(podNames) == 0 {
		return ""
	}
	parts := make([]string, len(podNames))
	for i, name := range podNames {
		parts[i] = regexp.QuoteMeta(name)
	}
	return "pod=~\"(?:" + strings.Join(parts, "|") + ")\","
}

// MemoryWorkingSetPromQL 构造 cAdvisor 内存工作集查询。
func MemoryWorkingSetPromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`container_memory_working_set_bytes{namespace=%q,container!="",container!="POD",%s}`,
		namespace, podMatcher(podNames))
}

// MemoryLimitPromQL 构造 kube-state-metrics 的内存 limit 查询。
func MemoryLimitPromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`kube_pod_container_resource_limits{namespace=%q,resource="memory",%s}`,
		namespace, podMatcher(podNames))
}

// CPUUsagePromQL 构造 CPU 使用速率（核/秒）查询。
func CPUUsagePromQL(namespace string, podNames []string) string {
	return fmt.Sprintf(
		`rate(container_cpu_usage_seconds_total{namespace=%q,container!="",container!="POD",%s}[5m])`,
		namespace, podMatcher(podNames))
}

// CPUThrottlePromQL 构造 CFS 限流时间片占比查询。
func CPUThrottlePromQL(namespace string, podNames []string) string {
	matcher := podMatcher(podNames)
	return fmt.Sprintf(
		`rate(container_cpu_cfs_throttled_periods_total{namespace=%q,container!="",%s}[5m]) `+
			`/ rate(container_cpu_cfs_periods_total{namespace=%q,container!="",%s}[5m])`,
		namespace, matcher, namespace, matcher)
}
