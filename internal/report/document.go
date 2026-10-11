// Package report 定义渲染无关的报告模型 Document，并提供 Markdown/HTML 渲染器。
// Document 同时是诊断历史落库与复现的序列化格式。
package report

import (
	"time"

	"opsagent/internal/diagnose"
)

// Document 是一份完整诊断报告的渲染无关表示。
type Document struct {
	Title         string
	TargetDisplay string // pod/p1、deployment/web、全部 Pod
	Namespace     string
	GeneratedAt   time.Time
	MetricSource  string // Prometheus 地址与来源，如 "discovery http://..."；空表示未用指标
	// Notices 是降级与校验提示，渲染在报告顶部。
	Notices  []string
	Findings []DocFinding
	LLM      *DocLLM // nil 表示本次未开启或未成功
	Overview []DocOverviewRow
	Evidence []DocEvidence
}

// DocFinding 是一条发现的视图数据，Explanation 已带行内 [ev-x] 标记。
type DocFinding struct {
	Title       string
	Severity    string
	Subject     string
	Explanation string
	Suggestion  string
	Risk        string
}

// DocLLM 是 LLM 分析的视图数据，InvalidCitations 为校验出的编造编号。
type DocLLM struct {
	Model            string
	ContentMD        string
	InvalidCitations []string
}

// DocOverviewRow 是资源概况中的一行。
type DocOverviewRow struct {
	Name   string
	Detail string
}

// DocEvidence 是一条证据的视图数据。
type DocEvidence struct {
	ID        string
	Kind      string
	Source    string
	Summary   string
	Detail    string
	Timestamp *time.Time
	Links     []diagnose.EvidenceLink
}
