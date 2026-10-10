package log

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLog_首次调用才建目录 惰性初始化的回归：包导入本身不得产生目录或文件。
// 本包只有这一个用例，测试二进制里没有别的用例会先调用 Log()。
func TestLog_首次调用才建目录(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	t.Setenv("FKA_LOG_DIR", dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("导入后、调用 Log() 前不应存在日志目录，stat 结果：%v", err)
	}

	if Log() == nil {
		t.Fatal("Log() 不应返回 nil")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("首次 Log() 之后日志目录应存在：%v", err)
	}
}
