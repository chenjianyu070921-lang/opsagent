package storage

import (
	"database/sql"
	"fmt"
)

// schemaV1 是第一个版本的表结构。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS diagnosis_records (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at       TEXT    NOT NULL,
  namespace        TEXT    NOT NULL,
  target_kind      TEXT    NOT NULL,
  target_name      TEXT    NOT NULL DEFAULT '',
  target_display   TEXT    NOT NULL,
  findings_count   INTEGER NOT NULL DEFAULT 0,
  highest_severity TEXT    NOT NULL DEFAULT '',
  duration_ms      INTEGER NOT NULL DEFAULT 0,
  prometheus_url   TEXT    NOT NULL DEFAULT '',
  llm_model        TEXT    NOT NULL DEFAULT '',
  schema_version   INTEGER NOT NULL,
  payload          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_diagnosis_created_at ON diagnosis_records (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_diagnosis_ns_time    ON diagnosis_records (namespace, created_at DESC);
`

// migrations 按版本号排列 DDL。
var migrations = map[int]string{
	1: schemaV1,
}

// migrate 读取 PRAGMA user_version，在事务内逐版本应用 DDL。
func (db *DB) migrate() error {
	var version int
	if err := db.sqlDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}
	for v := version + 1; v <= len(migrations); v++ {
		if err := applyVersion(db.sqlDB, v, migrations[v]); err != nil {
			return err
		}
	}
	return nil
}

// applyVersion 在一个事务内执行 DDL 并更新 user_version。
func applyVersion(sqlDB *sql.DB, version int, ddl string) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("开始迁移事务失败: %w", err)
	}
	if _, err := tx.Exec(ddl); err != nil {
		tx.Rollback()
		return fmt.Errorf("应用 v%d DDL 失败: %w", version, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
		tx.Rollback()
		return fmt.Errorf("更新数据库版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}
