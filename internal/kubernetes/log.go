package kubernetes

import (
	"context"
	"fmt"
	"io"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// maxLogBytes 限制单个容器取回的日志量，避免异常刷屏的日志把报告撑爆。
const maxLogBytes = 64 * 1024

// ContainerLog 是一个容器在指定时间窗口内的日志。
type ContainerLog struct {
	PodName       string
	ContainerName string
	Since         time.Duration
	TailLines     int64
	Content       string
	Truncated     bool
}

// GetContainerLogs 读取指定容器的日志尾部。
// since 限制日志的时间窗口，tailLines 限制最多返回的行数；两者为 0 时不限制。
func GetContainerLogs(ctx context.Context, client kubernetes.Interface, namespace, podName, containerName string, since time.Duration, tailLines int64) (*ContainerLog, error) {
	opts := &v1.PodLogOptions{Container: containerName}
	if since > 0 {
		secs := int64(since.Seconds())
		opts.SinceSeconds = &secs
	}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}

	req := client.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取 Pod %q 容器 %q 的日志失败: %w", podName, containerName, err)
	}
	defer stream.Close()

	// 多读 1 字节用于判断是否被截断。
	data, err := io.ReadAll(io.LimitReader(stream, maxLogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 Pod %q 容器 %q 的日志失败: %w", podName, containerName, err)
	}

	result := &ContainerLog{
		PodName:       podName,
		ContainerName: containerName,
		Since:         since,
		TailLines:     tailLines,
	}
	if int64(len(data)) > maxLogBytes {
		result.Content = string(data[:maxLogBytes])
		result.Truncated = true
	} else {
		result.Content = string(data)
	}
	return result, nil
}
