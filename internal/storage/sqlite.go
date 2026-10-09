// Package storage 用纯 Go SQLite 保存诊断历史。
// 本包不依赖 diagnose 等领域包：记录负载为 report.Document 的 JSON 字节。
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/glebarez/go-sqlite"
)

// DB 包装 SQLite 连接。
type DB struct {
	sqlDB *sql.DB
}

// Open 打开（不存在则创建）数据库，创建父目录并执行迁移。
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db := &DB{sqlDB: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// DefaultPath 返回默认数据库路径：os.UserConfigDir()/opsagent/history.db。
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户配置目录: %w", err)
	}
	return filepath.Join(dir, "opsagent", "history.db"), nil
}

// Close 关闭数据库连接。
func (db *DB) Close() error { return db.sqlDB.Close() }

// QueryRowForTest 暴露一行查询，仅供测试。
func (db *DB) QueryRowForTest(query string) *sql.Row {
	return db.sqlDB.QueryRow(query)
}
