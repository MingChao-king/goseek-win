package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goseek/internal/domain"
)

// bashCall 构造一次 bash 调用，参数由 command 和可选的 purpose 组成。
func bashCall(t *testing.T, command, purpose string) domain.ToolCall {
	t.Helper()
	arguments := map[string]string{"command": command}
	if purpose != "" {
		arguments["purpose"] = purpose
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	return domain.ToolCall{ID: "call-1", Name: "bash", Arguments: encoded}
}

// runBash 用给定超时执行一次调用，并断言不返回 Go error。
func runBash(t *testing.T, timeout time.Duration, call domain.ToolCall) domain.ToolResult {
	t.Helper()
	result, err := (&BashTool{timeout: timeout}).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回了 error，预期执行事实应当由 ToolResult 表达: %v", err)
	}
	return result
}

func TestRunReportsSuccessWithExitCodeZero(t *testing.T) {
	result := runBash(t, time.Minute, bashCall(t, "echo hello", ""))

	if result.Status != domain.ToolSuccess {
		t.Errorf("status = %q; want %q", result.Status, domain.ToolSuccess)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("exit_code = %v; want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Content) != "hello" {
		t.Errorf("content = %q; want %q", result.Content, "hello")
	}
}

// 非零退出是模型可读的观察，不是程序错误。
func TestRunReportsNonZeroExitAsErrorObservation(t *testing.T) {
	result := runBash(t, time.Minute, bashCall(t, "echo oops >&2; exit 3", ""))

	if result.Status != domain.ToolError {
		t.Errorf("status = %q; want %q", result.Status, domain.ToolError)
	}
	if result.ExitCode == nil || *result.ExitCode != 3 {
		t.Errorf("exit_code = %v; want 3", result.ExitCode)
	}
	if !strings.Contains(result.Content, "oops") {
		t.Errorf("stderr 没有被捕获: %q", result.Content)
	}
}

// stdout 与 stderr 共用一个 Writer，两路输出必须都在，且按时间顺序合并。
func TestRunMergesStdoutAndStderr(t *testing.T) {
	result := runBash(t, time.Minute, bashCall(t, "echo out; echo err >&2; echo tail", ""))

	for _, want := range []string{"out", "err", "tail"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content 缺少 %q: %q", want, result.Content)
		}
	}
	if index := strings.Index(result.Content, "out"); index > strings.Index(result.Content, "tail") {
		t.Errorf("输出顺序错乱: %q", result.Content)
	}
}

func TestRunRejectsInvalidArgumentsWithoutExecuting(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
	}{
		{"缺少 command", `{"purpose":"看看目录"}`},
		{"command 为空", `{"command":"   "}`},
		{"未知字段", `{"command":"echo hi","shell":"zsh"}`},
		{"参数不是 JSON 对象", `"echo hi"`},
		{"参数不是合法 JSON", `{command:`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call := domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(testCase.arguments)}
			result := runBash(t, time.Minute, call)

			if result.Status != domain.ToolError {
				t.Errorf("status = %q; want %q", result.Status, domain.ToolError)
			}
			if result.ExitCode != nil {
				t.Errorf("没有执行的调用带了退出码 %d", *result.ExitCode)
			}
			if !strings.Contains(result.Content, "没有执行") {
				t.Errorf("观察没有说明命令未执行: %q", result.Content)
			}
		})
	}
}

// 巨量输出**一个字节都不截**。
//
// 这条替代了上一版的 TestRunTruncatesHugeOutput。旧行为是超过 10000 字节就只留
// 头尾、中间挖掉并置 truncated 标记；那个设计已被推翻，理由见 tool/output.go。
func TestRunNeverTruncatesHugeOutput(t *testing.T) {
	// 每行 41 字节 × 20000 行 ≈ 820KB，是旧上限的 82 倍。
	const lines = 20000
	command := fmt.Sprintf(
		"for i in $(seq 1 %d); do echo 0123456789012345678901234567890123456789; done", lines)
	result := runBash(t, time.Minute, bashCall(t, command, ""))

	if result.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", result.Status, result.Content)
	}
	got := strings.Count(strings.TrimRight(result.Content, "\n"), "\n") + 1
	if got != lines {
		t.Errorf("收到 %d 行; want %d——输出被裁过", got, lines)
	}
	// 旧实现会插一条省略标记，现在绝不该出现。
	if strings.Contains(result.Content, "省略") {
		t.Error("输出里出现了省略标记，说明还有截断逻辑残留")
	}
}

// 超时必须终止整棵进程树：只杀直接子进程会留下继续运行的孙进程。
func TestRunKillsWholeProcessGroupOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	command := fmt.Sprintf("sleep 60 & echo $! > %s; sleep 60", pidFile)

	started := time.Now()
	result := runBash(t, 300*time.Millisecond, bashCall(t, command, ""))
	elapsed := time.Since(started)

	if result.Status != domain.ToolError {
		t.Errorf("status = %q; want %q", result.Status, domain.ToolError)
	}
	if !strings.Contains(result.Content, "超过") {
		t.Errorf("观察没有说明是超时: %q", result.Content)
	}
	if elapsed > 10*time.Second {
		t.Errorf("超时后等待了 %s 才返回", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("孙进程没有写下 PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("PID 文件内容无法解析: %v", err)
	}

	// Signal(0) 只做存在性检查：进程已退出时返回"进程找不到"类错误。
	// 抽成 probeProcess 是为了平台中立——syscall.Kill 在 Windows 上不存在。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = terminateProcess(pid)
	t.Fatalf("孙进程 %d 在超时后仍然存活", pid)
}

func TestDescribeCallPrefersPurposeAndFallsBack(t *testing.T) {
	cases := []struct {
		name string
		call domain.ToolCall
		want string
	}{
		{"有 purpose", bashCall(t, "ls", "查看桌面目录"), "查看桌面目录"},
		{"没有 purpose", bashCall(t, "ls", ""), "运行 Bash 命令"},
		{"参数不可解析", domain.ToolCall{Arguments: json.RawMessage(`{`)}, "运行 Bash 命令"},
		{"purpose 含换行", bashCall(t, "ls", "查看\n桌面"), "查看 桌面"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := (&BashTool{}).DescribeCall(testCase.call); got != testCase.want {
				t.Errorf("DescribeCall = %q; want %q", got, testCase.want)
			}
		})
	}
}

// purpose 只用于展示，绝不能进入 shell。
func TestPurposeIsNotPassedToTheShell(t *testing.T) {
	call := bashCall(t, "echo only-command", "echo leaked-purpose")
	result := runBash(t, time.Minute, call)

	if strings.Contains(result.Content, "leaked-purpose") {
		t.Errorf("purpose 被当成命令执行了: %q", result.Content)
	}
}

func TestSpecDeclaresCommandAsRequired(t *testing.T) {
	spec := NewBash("").Spec()
	if spec.Name != "bash" {
		t.Errorf("name = %q; want bash", spec.Name)
	}

	var schema struct {
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
		Properties           map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		t.Fatalf("参数 Schema 不是合法 JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "command" {
		t.Errorf("required = %v; want [command]", schema.Required)
	}
	if schema.AdditionalProperties {
		t.Error("Schema 允许了额外字段，与 DisallowUnknownFields 的实现不一致")
	}
	if _, ok := schema.Properties["purpose"]; !ok {
		t.Error("Schema 缺少 purpose")
	}
}

// 命令必须在会话自己的工作目录下执行，而不是进程的当前目录。
func TestRunExecutesInTheConfiguredWorkspace(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "只在这个目录里.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备标记文件失败: %v", err)
	}

	subject := &BashTool{workspace: workspace, timeout: time.Minute}
	result, err := subject.Run(context.Background(), bashCall(t, "ls 只在这个目录里.txt", ""), nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}

	if result.Status != domain.ToolSuccess {
		t.Fatalf("在工作目录下执行失败: %+v", result)
	}
	if !strings.Contains(result.Content, "只在这个目录里.txt") {
		t.Errorf("content = %q; want 含标记文件名", result.Content)
	}
}

// 工作目录为空时继承进程的当前目录，而不是执行失败。
func TestRunFallsBackToProcessDirectoryWhenWorkspaceIsEmpty(t *testing.T) {
	result := runBash(t, time.Minute, bashCall(t, "pwd", ""))

	if result.Status != domain.ToolSuccess {
		t.Fatalf("未设置工作目录时执行失败: %+v", result)
	}
	current, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd 失败: %v", err)
	}
	if strings.TrimSpace(result.Content) != current {
		t.Errorf("pwd = %q; want %q", strings.TrimSpace(result.Content), current)
	}
}

func TestRunDoesNotLoadLoginProfile(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".bash_profile")
	if err := os.WriteFile(profile, []byte("sleep 5\n"), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	subject := &BashTool{timeout: 500 * time.Millisecond}
	subject.SetEnv("HOME", home)
	result, err := subject.Run(
		context.Background(),
		bashCall(t, "echo ready", ""),
		nil,
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != domain.ToolSuccess || strings.TrimSpace(result.Content) != "ready" {
		t.Fatalf("login profile affected command startup: %+v", result)
	}
}

// 命令输出要边执行边交给回调，而不是等到结束才一次性给出。
func TestRunStreamsOutputWhileTheCommandIsStillRunning(t *testing.T) {
	var mutex sync.Mutex
	var chunks []string
	firstChunk := make(chan struct{})

	onOutput := func(chunk string) {
		mutex.Lock()
		defer mutex.Unlock()
		chunks = append(chunks, chunk)
		if len(chunks) == 1 {
			close(firstChunk)
		}
	}

	// 命令先输出一行，再睡一会儿才结束。如果输出是攒到最后才给的，
	// 下面的等待会超时。
	call := bashCall(t, "echo 第一行; sleep 1", "")
	done := make(chan domain.ToolResult, 1)
	go func() {
		result, err := (&BashTool{timeout: time.Minute}).Run(context.Background(), call, onOutput)
		if err != nil {
			t.Errorf("Run 返回错误: %v", err)
		}
		done <- result
	}()

	select {
	case <-firstChunk:
	case <-time.After(3 * time.Second):
		t.Fatal("命令结束前没有收到任何输出，说明输出没有流式交出")
	}

	result := <-done
	if result.Status != domain.ToolSuccess {
		t.Fatalf("命令没有成功: %+v", result)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if !strings.Contains(strings.Join(chunks, ""), "第一行") {
		t.Errorf("回调收到的内容 = %q", chunks)
	}
}

// 回调为 nil 是合法的：Registry 会兜住，工具自己也不该崩。
func TestRunToleratesNilOutputCallback(t *testing.T) {
	result, err := (&BashTool{timeout: time.Minute}).Run(
		context.Background(), bashCall(t, "echo hi", ""), nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess {
		t.Errorf("result = %+v", result)
	}
}
