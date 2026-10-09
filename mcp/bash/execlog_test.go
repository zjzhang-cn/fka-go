// 本文件钉执行日志：JSON Lines、按天轮转、**没跑的命令不留假退出码**。
//
// 最后一条最要紧：被策略拒 / 越界的尝试如果也写 `exit_code: 0`，读日志的人会把
// 一次被挡下的危险命令看成一次正常执行。
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readRecords 读出目录里唯一的执行日志文件，逐行解成记录。
func readRecords(t *testing.T, dir string) []execRecord {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读日志目录失败：%v", err)
	}
	var path string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), execLogPrefix) {
			path = filepath.Join(dir, entry.Name())
		}
	}
	if path == "" {
		t.Fatalf("没找到执行日志文件：%v", entries)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []execRecord
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec execRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("这行不是合法 JSON：%q（%v）", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestExecLog_写JSONLines并自动补时间 一条记录一行；Time 为空时自动补。
func TestExecLog_写JSONLines并自动补时间(t *testing.T) {
	dir := t.TempDir()
	l := NewExecLog(dir)
	code := 0
	l.Record(execRecord{Cwd: "sub", Command: "echo 你好\n世界", ExitCode: &code, DurationMS: 12})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	records := readRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("该只有一条记录，实际 %d", len(records))
	}
	rec := records[0]
	if rec.Command != "echo 你好\n世界" {
		t.Errorf("命令里的换行该原样保留到记录里，实际 %q", rec.Command)
	}
	if rec.Time == "" {
		t.Error("时间该被自动补上")
	}
	if rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Errorf("退出码该是 0，实际 %v", rec.ExitCode)
	}
}

// TestExecLog_没跑起来就没有exit_code 被拒的命令 exit_code 必须**整个缺席**。
func TestExecLog_没跑起来就没有exit_code(t *testing.T) {
	dir := t.TempDir()
	NewExecLog(dir).Record(execRecord{Command: "mount /dev/sda1", Error: "在内置黑名单里"})

	name := execLogPrefix + time.Now().UTC().Format(execLogDateLayout) + ".log"
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "exit_code") {
		t.Errorf("没跑的命令不该有 exit_code：%s", raw)
	}
	if !strings.Contains(string(raw), `"error"`) {
		t.Errorf("该记下被拒的原因：%s", raw)
	}
}

// TestExecLog_按UTC日期命名 文件名带 UTC 日期，与 internal/log 同一锚点。
func TestExecLog_按UTC日期命名(t *testing.T) {
	dir := t.TempDir()
	NewExecLog(dir).Record(execRecord{Command: "true"})

	name := execLogPrefix + time.Now().UTC().Format(execLogDateLayout) + ".log"
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("该落在 %s：%v", name, err)
	}
}

// TestExecLog_nil不panic execLog 可为 nil（测试不关心落盘时），Record 要能安全跳过。
func TestExecLog_nil不panic(t *testing.T) {
	var l *ExecLog
	l.Record(execRecord{Command: "true"})
}

// TestRun_执行后落一条日志 走完整 handler：命令跑了，日志里就该有命令与真实退出码。
func TestRun_执行后落一条日志(t *testing.T) {
	requireBash(t)

	dir := t.TempDir()
	impl := &bashServer{sandbox: newTestSandbox(t), execLog: NewExecLog(dir)}
	t.Cleanup(func() { _ = impl.execLog.Close() })

	if _, err := impl.handleRun(context.Background(),
		callRequest(map[string]any{"command": "echo hi"})); err != nil {
		t.Fatal(err)
	}

	records := readRecords(t, dir)
	if len(records) != 1 || records[0].Command != "echo hi" {
		t.Fatalf("该记下 echo hi 一条，实际 %+v", records)
	}
	if records[0].ExitCode == nil || *records[0].ExitCode != 0 {
		t.Errorf("退出码该是 0，实际 %v", records[0].ExitCode)
	}
}

// TestRun_被拒也落一条日志 被策略拒的命令也要留痕（error 字段），这正是审计要看
// 的「模型试过什么但被挡下」。
func TestRun_被拒也落一条日志(t *testing.T) {
	dir := t.TempDir()
	impl := &bashServer{sandbox: newTestSandbox(t), execLog: NewExecLog(dir)}
	t.Cleanup(func() { _ = impl.execLog.Close() })

	if _, err := impl.handleRun(context.Background(),
		callRequest(map[string]any{"command": "mount /dev/sda1 /mnt"})); err != nil {
		t.Fatal(err)
	}

	records := readRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("该记一条，实际 %+v", records)
	}
	if records[0].Error == "" {
		t.Error("被拒的命令该带 error 说明")
	}
	if records[0].ExitCode != nil {
		t.Errorf("被拒的命令不该有退出码，实际 %v", *records[0].ExitCode)
	}
}
