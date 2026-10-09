package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"opsagent/internal/report"
	"opsagent/internal/storage"
)

// jsonMarshalDoc 序列化 Document 作为历史 payload。
func jsonMarshalDoc(doc *report.Document) ([]byte, error) { return json.Marshal(doc) }

// bytesContains 判断 s 是否包含 sub。
func bytesContains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// osReadFile 读取整个文件。
func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// idToString 把记录 id 转成命令行使用的字符串。
func idToString(id int64) string { return strconv.FormatInt(id, 10) }

// seedHistory 在临时 DB 中写入一条带 Document payload 的记录。
func seedHistory(t *testing.T, ns string) (dbPath string, id int64) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "history.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doc := &report.Document{
		Title: "诊断报告", TargetDisplay: "pod/p1", Namespace: ns, GeneratedAt: time.Now(),
		Findings: []report.DocFinding{{Title: "测试故障", Severity: "严重", Explanation: "分析"}},
	}
	payload, err := jsonMarshalDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	id, err = db.Save(t.Context(), storage.Record{
		CreatedAt: time.Now(), Namespace: ns, TargetKind: "pod", TargetName: "p1",
		TargetDisplay: "pod/p1", FindingsCount: 1, HighestSeverity: "严重",
		SchemaVersion: 1, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dbPath, id
}

func executeWithHistoryDB(args []string, dbPath string) (stdout, stderr bytes.Buffer, err error) {
	rootCmd.SetArgs(append(args, "--history-db", dbPath))
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	err = rootCmd.Execute()
	rootCmd.SetArgs(nil)
	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)
	return stdout, stderr, err
}

func TestHistoryList(t *testing.T) {
	dbPath, _ := seedHistory(t, "default")
	stdout, _, err := executeWithHistoryDB([]string{"history", "list"}, dbPath)
	if err != nil {
		t.Fatalf("history list 失败: %v", err)
	}
	got := stdout.String()
	if !bytesContains(got, "ID") || !bytesContains(got, "pod/p1") {
		t.Errorf("history list 输出缺少表头或目标: %q", got)
	}
}

// seedHistoryAt 在已存在的 dbPath 中追加一条指定命名空间与对象的记录。
func seedHistoryAt(t *testing.T, dbPath, ns, targetName string) {
	t.Helper()
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	display := "pod/" + targetName
	doc := &report.Document{
		Title: "诊断报告", TargetDisplay: display, Namespace: ns, GeneratedAt: time.Now(),
		Findings: []report.DocFinding{{Title: "测试故障", Severity: "严重", Explanation: "分析"}},
	}
	payload, err := jsonMarshalDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Save(t.Context(), storage.Record{
		CreatedAt: time.Now(), Namespace: ns, TargetKind: "pod", TargetName: targetName,
		TargetDisplay: display, FindingsCount: 1, HighestSeverity: "严重",
		SchemaVersion: 1, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryListByNamespace 验证不带 -n 时历史列表不过滤命名空间、带 -n 时按值过滤。
func TestHistoryListByNamespace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	seedHistoryAt(t, dbPath, "default", "p1")
	seedHistoryAt(t, dbPath, "production", "p2")

	// 不带 -n：应同时包含两个命名空间的记录。
	stdout, _, err := executeWithHistoryDB([]string{"history", "list"}, dbPath)
	if err != nil {
		t.Fatalf("history list 失败: %v", err)
	}
	got := stdout.String()
	if !bytesContains(got, "pod/p1") || !bytesContains(got, "pod/p2") {
		t.Errorf("不带 -n 的 history list 应包含两个命名空间的记录: %q", got)
	}

	// 带 -n production：只应包含 production 记录，不含 default。
	stdout, _, err = executeWithHistoryDB([]string{"history", "list", "-n", "production"}, dbPath)
	if err != nil {
		t.Fatalf("history list -n production 失败: %v", err)
	}
	got = stdout.String()
	if !bytesContains(got, "pod/p2") || bytesContains(got, "pod/p1") {
		t.Errorf("history list -n production 应只含 production 记录、不含 default: %q", got)
	}
}

// TestOpenHistoryRepoBestEffort 覆盖尽力开库辅助的两条路径：合法路径成功、非法路径返回 notice。
func TestOpenHistoryRepoBestEffort(t *testing.T) {
	t.Run("合法路径返回仓储且无 notice", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "history.db")
		repo, notice := openHistoryRepoBestEffort(dbPath)
		if repo == nil {
			t.Fatalf("合法路径应返回非 nil 仓储")
		}
		defer repo.(*storage.DB).Close()
		if notice != "" {
			t.Errorf("合法路径 notice 应为空，实际 %q", notice)
		}
	})

	t.Run("非法路径返回 nil 与「历史数据库不可用」notice", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("block"), 0o644); err != nil {
			t.Fatal(err)
		}
		// MkdirAll 的父目录是普通文件，必然失败。
		dbPath := filepath.Join(blocker, "history.db")
		repo, notice := openHistoryRepoBestEffort(dbPath)
		if repo != nil {
			repo.(*storage.DB).Close()
			t.Fatalf("非法路径应返回 nil 仓储")
		}
		if !strings.Contains(notice, "历史数据库不可用") {
			t.Errorf("notice 应包含「历史数据库不可用」，实际 %q", notice)
		}
	})
}

func TestHistoryShow(t *testing.T) {
	dbPath, id := seedHistory(t, "default")
	stdout, _, err := executeWithHistoryDB([]string{"history", "show", idToString(id)}, dbPath)
	if err != nil {
		t.Fatalf("history show 失败: %v", err)
	}
	got := stdout.String()
	if !bytesContains(got, "# 诊断报告") || !bytesContains(got, "测试故障") {
		t.Errorf("history show 未复现报告: %q", got)
	}
}

func TestHistoryExport(t *testing.T) {
	dbPath, id := seedHistory(t, "default")
	exportFile := filepath.Join(t.TempDir(), "report.html")
	_, _, err := executeWithHistoryDB(
		[]string{"history", "export", idToString(id), "-o", exportFile}, dbPath)
	if err != nil {
		t.Fatalf("history export 失败: %v", err)
	}
	data, err := osReadFile(exportFile)
	if err != nil {
		t.Fatalf("读取导出文件失败: %v", err)
	}
	if !bytesContains(string(data), "<!doctype html>") {
		t.Errorf("导出内容不是 HTML: %q", string(data)[:200])
	}
}

func TestHistoryShowUnknownID(t *testing.T) {
	dbPath, _ := seedHistory(t, "default")
	_, _, err := executeWithHistoryDB([]string{"history", "show", "999"}, dbPath)
	if err == nil {
		t.Errorf("show 不存在的 id 应报错")
	}
}
