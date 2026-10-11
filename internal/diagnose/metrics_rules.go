package diagnose

import (
	"fmt"
	"strings"
	"time"

	"opsagent/internal/prometheus"
)

// --- 内存压力规则 ---

// memoryTrendRule 检查每个容器的内存工作集峰值占比，≥85% 时报内存压力。
func memoryTrendRule(snap *Snapshot, set *EvidenceSet) []Finding {
	if len(snap.Metrics) == 0 {
		return nil
	}
	end := snap.CollectedAt
	start := end.Add(-snap.MetricsWindow)

	var findings []Finding
	for podName, containers := range snap.Metrics {
		for containerName, m := range containers {
			if m == nil || m.MemoryLimit == nil || *m.MemoryLimit <= 0 {
				continue
			}
			peak := peakValue(m.MemoryWorkingSet)
			ratio := peak / *m.MemoryLimit
			m.MemoryPeakRatio = ratio
			if ratio < 0.85 {
				continue
			}

			promQL := prometheus.MemoryWorkingSetPromQL(snap.Target.Namespace, []string{podName})
			summary := fmt.Sprintf("容器 %s 内存工作集峰值占 limit 的 %.0f%%", containerName, ratio*100)
			id := set.AddWith(Evidence{
				Kind:      "metric",
				Source:    containerSource(podName, containerName),
				Summary:   summary,
				Detail:    pointsText(m.MemoryWorkingSet),
				Timestamp: &snap.CollectedAt,
				Links:     metricLinks(snap, promQL, start, end),
			})

			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 内存使用逼近上限（峰值 %.0f%%）", containerName, ratio*100),
				Category:    "memory-pressure",
				Severity:    SeverityMedium,
				Subject:     containerSource(podName, containerName),
				Explanation: fmt.Sprintf("过去 %s 内存工作集峰值达到 limit 的 %.0f%%，已接近 OOM 临界。", snap.MetricsWindow, ratio*100),
				Suggestion:  "结合趋势判断是缓慢增长（排查内存泄漏、缓存/大对象、堆参数）还是真实业务增长；确认根因后再调整 limit。",
				Risk:        "使用继续增长将触发 OOMKill；未确认根因前不要直接调高 limit。",
				EvidenceIDs: []string{id},
			})
		}
	}
	return findings
}

// --- CPU 限流规则 ---

// cpuThrottleRule 检查 CFS 限流时间片比率均值，≥25% 时报 CPU 限流。
func cpuThrottleRule(snap *Snapshot, set *EvidenceSet) []Finding {
	if len(snap.Metrics) == 0 {
		return nil
	}
	end := snap.CollectedAt
	start := end.Add(-snap.MetricsWindow)

	var findings []Finding
	for podName, containers := range snap.Metrics {
		for containerName, m := range containers {
			if m == nil || len(m.CPUThrottledRatio) == 0 {
				continue
			}
			avg := averageValue(m.CPUThrottledRatio)
			if avg < 0.25 {
				continue
			}

			promQL := prometheus.CPUThrottlePromQL(snap.Target.Namespace, []string{podName})
			summary := fmt.Sprintf("容器 %s CPU 限流时间片平均占比 %.0f%%", containerName, avg*100)
			id := set.AddWith(Evidence{
				Kind:      "metric",
				Source:    containerSource(podName, containerName),
				Summary:   summary,
				Detail:    pointsText(m.CPUThrottledRatio),
				Timestamp: &snap.CollectedAt,
				Links:     metricLinks(snap, promQL, start, end),
			})

			findings = append(findings, Finding{
				Title:       fmt.Sprintf("容器 %s 出现 CPU 限流（平均 %.0f%%）", containerName, avg*100),
				Category:    "cpu-throttle",
				Severity:    SeverityLow,
				Subject:     containerSource(podName, containerName),
				Explanation: fmt.Sprintf("过去 %s 内约 %.0f%% 的 CPU 调度时间片被限流，进程申请的 CPU 超过了 limit。", snap.MetricsWindow, avg*100),
				Suggestion:  "确认应用是否存在计算尖峰；若长期接近 limit，适当调大 CPU limit 或优化热点逻辑。",
				Risk:        "持续限流会拉长请求延迟、放大超时；盲目调大 limit 会挤占节点容量。",
				EvidenceIDs: []string{id},
			})
		}
	}
	return findings
}

// --- 辅助函数 ---

// metricFor 返回快照中指定容器的指标（不存在返回 nil）。
func metricFor(snap *Snapshot, podName, containerName string) *prometheus.ContainerMetrics {
	if containers, ok := snap.Metrics[podName]; ok {
		return containers[containerName]
	}
	return nil
}

// metricLinks 构造指标证据的 graph/grafana 链接。
func metricLinks(snap *Snapshot, promQL string, start, end time.Time) []EvidenceLink {
	links := []EvidenceLink{{
		Type: "graph", Label: "Prometheus 图表",
		URL: prometheus.GraphURL(snap.PrometheusURL, promQL, start, end),
	}}
	if g := prometheus.GrafanaExploreURL(snap.GrafanaURL, promQL, start, end); g != "" {
		links = append(links, EvidenceLink{Type: "grafana", Label: "Grafana Explore", URL: g})
	}
	return links
}

// pointsText 把指标点序列渲染成文本，最多保留最后 20 个点。
func pointsText(points []prometheus.Point) string {
	if len(points) > 20 {
		points = points[len(points)-20:]
	}
	lines := make([]string, 0, len(points))
	for _, p := range points {
		lines = append(lines, fmt.Sprintf("%s  %g", p.T.Format("2006-01-02 15:04:05"), p.V))
	}
	return strings.Join(lines, "\n")
}

// peakValue 返回点序列的最大值。
func peakValue(points []prometheus.Point) float64 {
	peak := 0.0
	for _, p := range points {
		if p.V > peak {
			peak = p.V
		}
	}
	return peak
}

// averageValue 返回点序列的算术平均值。
func averageValue(points []prometheus.Point) float64 {
	if len(points) == 0 {
		return 0
	}
	sum := 0.0
	for _, p := range points {
		sum += p.V
	}
	return sum / float64(len(points))
}
