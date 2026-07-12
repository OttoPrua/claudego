package main

// codex 失败可靠性回归：codexErrorLine 必须跨过 codex 横幅/配置噪声取真错误，
// transientRe 必须认得 codex 的瞬时网络错误——否则失败一律显示 "Reading additional input from stdin"
// 且被当硬失败烧 attempts，单腿审核一挂就没了。

import (
	"strings"
	"testing"
)

// 真实 codex exec 失败输出的形态（横幅在第一行，真错误在末尾）。
const codexFailOutput = `Reading additional input from stdin...
OpenAI Codex v0.144.1
--------
workdir: /x
model: mock-model
provider: openai
approval: never
sandbox: read-only
reasoning effort: xhigh
session id: 019f-...
--------
user
审核一下这个改动
stream error: stream disconnected before completion
`

func TestCodexErrorLineSkipsBanner(t *testing.T) {
	got := codexErrorLine(codexFailOutput)
	// 必须取到真错误,绝不能是横幅。
	if strings.Contains(got, "Reading additional input") || strings.HasPrefix(got, "OpenAI Codex") {
		t.Fatalf("codexErrorLine 取到了横幅噪声而非真错误: %q", got)
	}
	if !strings.Contains(got, "stream disconnected") {
		t.Fatalf("codexErrorLine 应取到真错误 'stream disconnected', got %q", got)
	}
	// 且提取出的真错误应被 transientRe 认作瞬时错误（→ 退避重试而非硬失败）。
	if !transientRe.MatchString(got) {
		t.Fatalf("提取的 codex 网络错误应匹配 transientRe（否则被当硬失败）: %q", got)
	}
}

// 老 bug 实证：横幅本身不匹配 transientRe——所以 firstLine 取横幅时,瞬时错误被误判硬失败。
func TestBannerNotTransient(t *testing.T) {
	if transientRe.MatchString("Reading additional input from stdin...") {
		t.Fatal("横幅不该匹配 transientRe（否则测试构造无意义）")
	}
	// 而 firstLine 恰好会取到横幅——这正是被修的 bug。
	if fl := firstLine(codexFailOutput); !strings.Contains(fl, "Reading additional input") {
		t.Fatalf("前提:firstLine 取到横幅(旧 bug), got %q", fl)
	}
}

func TestTransientMatchesCodexNetErrors(t *testing.T) {
	transient := []string{
		"stream error: stream disconnected before completion",
		"error sending request to https://...: connection reset by peer",
		"connection refused",
		"503 Service Unavailable",
		"read tcp 10.0.0.1:443: i/o timeout",
		"request timed out",
	}
	for _, s := range transient {
		if !transientRe.MatchString(s) {
			t.Fatalf("应认作瞬时错误(退避重试): %q", s)
		}
	}
	// 反例守卫：真正的硬错误不应被误判为瞬时（否则永远重试不失败）。
	for _, s := range []string{"invalid api key", "model not supported", "permission denied writing file"} {
		if transientRe.MatchString(s) {
			t.Fatalf("硬错误不该匹配 transientRe: %q", s)
		}
	}
}

// 只有噪声、无真错误行时,回退到最后一条非噪声行(而非空/横幅)。
func TestCodexErrorLineFallback(t *testing.T) {
	onlyNoise := "Reading additional input from stdin...\nOpenAI Codex v0.144.1\n--------\nmodel: x\n"
	got := codexErrorLine(onlyNoise)
	if strings.Contains(got, "Reading additional input") {
		t.Fatalf("全噪声时也不该回退到横幅, got %q", got)
	}
}
