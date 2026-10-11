package cli

import (
	"path/filepath"
	"testing"
)

func TestExportFormat(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		explicit string
		want     string
		wantErr  bool
	}{
		{"扩展名 md", "a.md", "", "markdown", false},
		{"扩展名 html", "a.html", "", "html", false},
		{"format 优先", "a.md", "html", "html", false},
		{"未知扩展名", "a.txt", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exportFormat(tc.path, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Errorf("期望报错")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("exportFormat = %q, %v，期望 %q", got, err, tc.want)
			}
		})
	}
}

func TestLastMutuallyExclusive(t *testing.T) {
	if err := validateLastArgs(true, []string{"pod/p1"}); err == nil {
		t.Errorf("--last 与诊断目标同时给出应报错")
	}
	if err := validateLastArgs(false, []string{"pod/p1"}); err != nil {
		t.Errorf("正常诊断不应报错: %v", err)
	}
	if err := validateLastArgs(true, nil); err != nil {
		t.Errorf("单独 --last 不应报错: %v", err)
	}
}

func TestLastEmptyHistory(t *testing.T) {
	// 空库 --last 应返回明确错误（用临时 DB 路径）。
	rootCmd.SetArgs([]string{"diagnose", "--last", "--history-db", emptyDBPath(t)})
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	if err == nil {
		t.Errorf("空库 --last 应返回错误")
	}
}

func emptyDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "history.db")
}
