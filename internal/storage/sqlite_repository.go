package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 确保 *DB 实现 Repository。
var _ Repository = (*DB)(nil)

// Save 插入一条记录，返回自增 id。时间统一以 RFC3339 UTC 存储。
func (db *DB) Save(ctx context.Context, r Record) (int64, error) {
	createdAt := r.CreatedAt.UTC().Format(time.RFC3339)
	res, err := db.sqlDB.ExecContext(ctx, `
INSERT INTO diagnosis_records
  (created_at, namespace, target_kind, target_name, target_display,
   findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
   schema_version, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		createdAt, r.Namespace, r.TargetKind, r.TargetName, r.TargetDisplay,
		r.FindingsCount, r.HighestSeverity, r.DurationMS, r.PrometheusURL, r.LLMModel,
		r.SchemaVersion, string(r.Payload))
	if err != nil {
		return 0, fmt.Errorf("保存诊断记录失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取记录 id 失败: %w", err)
	}
	return id, nil
}

// List 返回摘要列表，可按命名空间过滤，按 created_at 倒序；limit<=0 时默认 50。
func (db *DB) List(ctx context.Context, namespace string, limit int) ([]RecordSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model
FROM diagnosis_records`
	args := []interface{}{}
	if namespace != "" {
		query += ` WHERE namespace = ?`
		args = append(args, namespace)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询诊断历史失败: %w", err)
	}
	defer rows.Close()

	var out []RecordSummary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get 按 id 取完整记录。
func (db *DB) Get(ctx context.Context, id int64) (*Record, error) {
	row := db.sqlDB.QueryRowContext(ctx, `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
       schema_version, payload
FROM diagnosis_records WHERE id = ?`, id)
	return scanRecord(row)
}

// Latest 取最近一条完整记录。
func (db *DB) Latest(ctx context.Context) (*Record, error) {
	row := db.sqlDB.QueryRowContext(ctx, `
SELECT id, created_at, namespace, target_kind, target_name, target_display,
       findings_count, highest_severity, duration_ms, prometheus_url, llm_model,
       schema_version, payload
FROM diagnosis_records ORDER BY created_at DESC LIMIT 1`)
	return scanRecord(row)
}

// rowScanner 同时兼容 sql.Row 与 sql.Rows。
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// scanSummary 扫描一行摘要。
func scanSummary(s rowScanner) (RecordSummary, error) {
	var (
		summary   RecordSummary
		createdAt string
		promURL   string
		llmModel  string
	)
	err := s.Scan(&summary.ID, &createdAt, &summary.Namespace, &summary.TargetKind,
		&summary.TargetName, &summary.TargetDisplay, &summary.FindingsCount,
		&summary.HighestSeverity, &summary.DurationMS, &promURL, &llmModel)
	if err != nil {
		return RecordSummary{}, err
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return RecordSummary{}, fmt.Errorf("解析记录时间失败: %w", err)
	}
	summary.CreatedAt = t
	summary.PrometheusURL = promURL
	summary.LLMModel = llmModel
	return summary, nil
}

// scanRecord 扫描一条完整记录，无行时返回 ErrNotFound。
func scanRecord(s rowScanner) (*Record, error) {
	var (
		rec       Record
		createdAt string
		payload   string
	)
	err := s.Scan(&rec.ID, &createdAt, &rec.Namespace, &rec.TargetKind,
		&rec.TargetName, &rec.TargetDisplay, &rec.FindingsCount,
		&rec.HighestSeverity, &rec.DurationMS, &rec.PrometheusURL, &rec.LLMModel,
		&rec.SchemaVersion, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取诊断记录失败: %w", err)
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("解析记录时间失败: %w", err)
	}
	rec.CreatedAt = t
	rec.Payload = []byte(payload)
	return &rec, nil
}
