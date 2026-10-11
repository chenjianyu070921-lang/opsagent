package storage

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示按 id 查询或空库取最近记录时无数据。
var ErrNotFound = errors.New("诊断记录不存在")

// Record 是一次诊断的完整持久化记录。
type Record struct {
	ID              int64
	CreatedAt       time.Time
	Namespace       string
	TargetKind      string
	TargetName      string
	TargetDisplay   string
	FindingsCount   int
	HighestSeverity string
	DurationMS      int64
	PrometheusURL   string
	LLMModel        string
	SchemaVersion   int
	Payload         []byte
}

// RecordSummary 是列表用的摘要，不含 payload。
type RecordSummary struct {
	ID              int64
	CreatedAt       time.Time
	Namespace       string
	TargetKind      string
	TargetName      string
	TargetDisplay   string
	FindingsCount   int
	HighestSeverity string
	DurationMS      int64
	PrometheusURL   string
	LLMModel        string
}

// Repository 是诊断历史的持久化边界。
type Repository interface {
	Save(ctx context.Context, r Record) (int64, error)
	List(ctx context.Context, namespace string, limit int) ([]RecordSummary, error)
	Get(ctx context.Context, id int64) (*Record, error)
	Latest(ctx context.Context) (*Record, error)
}
