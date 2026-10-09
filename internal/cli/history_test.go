package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
