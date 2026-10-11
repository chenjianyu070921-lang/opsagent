package storage_test

import (
	"path/filepath"
	"strings"
	"testing"

	"opsagent/internal/storage"
)

func TestOpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// user_version 应为 1。
	var version int
	if err := db.QueryRowForTest(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("查询 user_version 失败: %v", err)
	}
	if version != 1 {
		t.Errorf("迁移后 user_version 应为 1，实际 %d", version)
	}

	// diagnosis_records 表必须存在。
	var name string
	err = db.QueryRowForTest(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='diagnosis_records'`).Scan(&name)
	if err != nil || name != "diagnosis_records" {
		t.Errorf("diagnosis_records 表未创建: %v", err)
	}
}

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db1, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := storage.Open(path) // 再次打开不应重复迁移报错
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	db2.Close()
}

func TestDefaultPath(t *testing.T) {
	got, err := storage.DefaultPath()
	if err != nil {
		t.Fatalf("默认路径: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(got), "opsagent/history.db") {
		t.Errorf("默认路径应以 opsagent/history.db 结尾，实际 %q", got)
	}
}
