package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// TypeDefaults 是某一任务类型的默认执行参数，在 add/emit 时烘焙进任务。
type TypeDefaults struct {
	PermissionMode  string   `json:"permission_mode,omitempty"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	SkipPermissions bool     `json:"skip_permissions,omitempty"`
	// Model 该类型默认使用的模型（--model 值，如 haiku/sonnet），空表示账号默认模型。
	Model string `json:"model,omitempty"`
}

type Config struct {
	ClaudeBin         string                  `json:"claude_bin"`
	PollIntervalSec   int                     `json:"poll_interval_sec"`
	LimitFallbackMin  int                     `json:"limit_fallback_min"`
	CooldownMarginSec int                     `json:"cooldown_margin_sec"`
	StepTimeoutMin    int                     `json:"step_timeout_min"`
	MaxAttempts       int                     `json:"max_attempts_per_step"`
	RetryBackoffMin   int                     `json:"retry_backoff_min"`
	// MaxParallel: 单次 tick 内最多并行跑几个任务（同一工作目录始终串行）。1 为纯串行。
	MaxParallel int  `json:"max_parallel"`
	ResumeFirst bool `json:"resume_first"`
	TypeOrder         []string                `json:"type_order"`
	ResumePrompt      string                  `json:"resume_prompt"`
	TypeDefaults      map[string]TypeDefaults `json:"type_defaults"`

	// ---- 5 小时额度红线（保底额度，给交互/突发任务留余量）----
	// QueueBudgetTokens: 滑动 5 小时窗口内，队列最多消耗的加权 token 数；0 关闭。
	// 只统计 claudego 自己派发的调用（桌面端消耗不可见），本质是"队列预算上限"。
	QueueBudgetTokens int64 `json:"queue_budget_tokens"`
	// RedlinePercent + UsageFeed: 外部全局用量源（CodexBar usage-history.jsonl 格式），
	// 最新 claude 5h 窗口样本 usedPercent 达到红线即停止派发；样本过期则放行（fail-open）。
	RedlinePercent     int    `json:"redline_percent"`
	UsageFeed          string `json:"usage_feed"`
	UsageFeedMaxAgeMin int    `json:"usage_feed_max_age_min"`
	// ModelWeights: 各模型 token 的额度权重（订阅限额按模型加权），键为 --model 值，"default" 兜底。
	ModelWeights map[string]float64 `json:"model_weights"`
	// RedlineWindows: 分时段红线。时段内非零字段覆盖全局阈值，时段外回落全局配置。
	RedlineWindows []RedlineWindow `json:"redline_windows"`
	// RedlineLeadMin: 红线时段的前置缓冲（分钟）。时段开始前这么多分钟就停发 claude 任务，
	// 防止起跑的长任务踩进预留窗口（单步任务无法中途让位）。codex 钉定任务不受影响。
	RedlineLeadMin int `json:"redline_lead_min,omitempty"`

	// ---- Codex 备用执行器（claude 冷却/红线期间不断档）----
	// CodexBin 非空且 CodexFallback 开启时：claude 被冷却或红线拦住的时段，
	// 把"单步、无既有 claude 会话"的任务（协调/审核/装配/单步 add）切给 codex exec 执行；
	// 带会话的多步任务仍等 claude 重置（跨 CLI 无法延续上下文）。
	CodexBin      string `json:"codex_bin"`
	CodexFallback bool   `json:"codex_fallback"`
	CodexModel    string `json:"codex_model,omitempty"`
	// CodexReasoning 透传 -c model_reasoning_effort（minimal/low/medium/high/xhigh），空则用 codex 默认。
	CodexReasoning string `json:"codex_reasoning,omitempty"`
	// NoFallbackModels：这些模型的任务在 claude 冷却/红线期不降级到 codex，宁可排队等。
	// 设计类卡（fable）质量优先——降级执行violates分层原则。runner_pref 钉定不受此限。
	NoFallbackModels []string `json:"no_fallback_models,omitempty"`
	// ThinkingTokens >0 时给 claude 调用设置 MAX_THINKING_TOKENS（拉高思考预算，设计类任务受益）。
	ThinkingTokens int `json:"thinking_tokens,omitempty"`
}

// RedlineWindow 是按每日本地时间生效的红线时段（"HH:MM"，跨零点用 from > to 表示）。
type RedlineWindow struct {
	From              string `json:"from"`
	To                string `json:"to"`
	RedlinePercent    int    `json:"redline_percent,omitempty"`
	QueueBudgetTokens int64  `json:"queue_budget_tokens,omitempty"`
}

const (
	typeReview       = "design-review"
	typeAssembly     = "prompt-assembly"
	typeSequence     = "sequence"
	typeCoordinate   = "coordinate"    // 分工协调：读队列+进度报告，产出任务分工并自动入队
	typeProgressPull = "progress-pull" // 进度回收：--resume 某会话，让它输出结构化进度报告
)

func defaultConfig(claudeBin string) *Config {
	return &Config{
		ClaudeBin:         claudeBin,
		PollIntervalSec:   300,
		LimitFallbackMin:  30,
		CooldownMarginSec: 90,
		StepTimeoutMin:    60,
		MaxAttempts:       3,
		RetryBackoffMin:   5,
		MaxParallel:       1,
		ResumeFirst:       true,
		TypeOrder:         []string{typeProgressPull, typeCoordinate, typeReview, typeSequence, typeAssembly},
		QueueBudgetTokens: 0,
		RedlinePercent:    0,
		UsageFeedMaxAgeMin: 90,
		ModelWeights: map[string]float64{
			"default": 1, "opus": 5, "sonnet": 1, "haiku": 0.2, "claude-fable-5": 10, "fable": 10,
		},
		NoFallbackModels: []string{"claude-fable-5", "fable"},
		ResumePrompt:      "继续。上一条指令因为用量限额被中断，请从中断的地方接着完成当前任务；如果其实已经完成了，请直接说明完成情况。",
		TypeDefaults: map[string]TypeDefaults{
			typeReview: {
				PermissionMode: "default",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeAssembly: {
				PermissionMode: "default",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			// 协调是全队列的编排大脑：小 token 量、高杠杆，用最强模型（预算紧可改 sonnet）；
			// 进度回收是机械总结，haiku 即可。
			typeCoordinate: {
				PermissionMode: "default",
				Model:          "opus",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeProgressPull: {
				PermissionMode: "default",
				Model:          "haiku",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(git diff:*)",
				},
			},
			typeSequence: {
				PermissionMode: "acceptEdits",
				AllowedTools: []string{
					"Read", "Grep", "Glob", "Edit", "Write", "MultiEdit", "Task",
					"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
					"Bash(mkdir:*)", "Bash(ls:*)",
					"Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)",
					"Bash(npm run:*)", "Bash(npm test:*)", "Bash(pnpm run:*)", "Bash(pnpm test:*)",
					"Bash(python3 -m pytest:*)",
				},
			},
		},
	}
}

func configPath(root string) string    { return filepath.Join(root, "config.json") }
func tasksDir(root string) string      { return filepath.Join(root, "tasks") }
func progressDir(root string) string   { return filepath.Join(root, "progress") }
func archiveDir(root string) string    { return filepath.Join(root, "archive") }
func logsDir(root string) string       { return filepath.Join(root, "logs") }
func templatesDir(root string) string  { return filepath.Join(root, "templates") }
func cooldownPath(root string) string  { return filepath.Join(root, "cooldown.json") }
func usagePath(root string) string     { return filepath.Join(root, "usage.json") }
func lockPath(root string) string      { return filepath.Join(root, ".lock") }
func taskLogPath(root, id string) string { return filepath.Join(logsDir(root), id+".log") }

func defaultRoot() string {
	if v := os.Getenv("CLAUDEGO_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claudego"
	}
	return filepath.Join(home, ".claudego")
}

func resolveRoot(flagVal string) string {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err == nil {
			return abs
		}
		return flagVal
	}
	return defaultRoot()
}

func loadConfig(root string) (*Config, error) {
	data, err := os.ReadFile(configPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到 %s，请先运行: claudego init", configPath(root))
		}
		return nil, err
	}
	cfg := defaultConfig("claude")
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", configPath(root), err)
	}
	return cfg, nil
}

func saveConfig(root string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(configPath(root), append(data, '\n'))
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
