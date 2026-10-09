package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"opsagent/internal/storage"
)

func sampleRecord(createdAt time.Time, namespace string) storage.Record {
	return storage.Record{
		CreatedAt:       createdAt,
		Namespace:       namespace,
		TargetKind:      "pod",
		TargetName:      "p1",
		TargetDisplay:   "pod/p1",
		FindingsCount:   2,
		HighestSeverity: "严重",
		DurationMS:      123,
		PrometheusURL:   "http://prom:9090",
		LLMModel:        "test-model",
		SchemaVersion:   1,
		Payload:         []byte(`{"title":"诊断报告"}`),
	}
}

func openTestDB(t *testing.T) storage.Repository {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSaveAndGet(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	created := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	id, err := repo.Save(ctx, sampleRecord(created, "default"))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if id != 1 {
		t.Errorf("首条记录 id 应为 1，实际 %d", id)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Namespace != "default" || got.TargetDisplay != "pod/p1" || got.FindingsCount != 2 {
		t.Errorf("标量字段往返错误: %+v", got)
	}
	if string(got.Payload) != `{"title":"诊断报告"}` {
		t.Errorf("payload 往返错误: %s", got.Payload)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("时间往返错误: %v", got.CreatedAt)
	}
}

func TestListSummaryAndFilter(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	if _, err := repo.Save(ctx, sampleRecord(time.Now().Add(-2*time.Hour), "default")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Save(ctx, sampleRecord(time.Now().Add(-time.Hour), "production")); err != nil {
		t.Fatal(err)
	}

	all, err := repo.List(ctx, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应有 2 条摘要，实际 %d", len(all))
	}
	// 按 created_at 倒序：production 记录更新，应排第一。
	if all[0].Namespace != "production" {
		t.Errorf("排序应为时间倒序，实际首条 %q", all[0].Namespace)
	}
	// 摘要不含 payload 由 RecordSummary 类型（无 Payload 字段）静态保证。

	prod, err := repo.List(ctx, "production", 50)
	if err != nil || len(prod) != 1 || prod[0].Namespace != "production" {
		t.Errorf("namespace 过滤错误: %v %d", err, len(prod))
	}
}

func TestLatest(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	if _, err := repo.Latest(ctx); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("空库 Latest 应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := repo.Save(ctx, sampleRecord(time.Now(), "default")); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Latest(ctx)
	if err != nil {
		t.Fatalf("非空库 Latest 失败: %v", err)
	}
	if got.TargetDisplay != "pod/p1" {
		t.Errorf("Latest 内容错误: %+v", got)
	}
}

func TestGetUnknownID(t *testing.T) {
	repo := openTestDB(t)
	_, err := repo.Get(context.Background(), 999)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("未知 ID 应返回 ErrNotFound，实际 %v", err)
	}
}
