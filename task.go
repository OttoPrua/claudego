package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	statusQueued      = "queued"
	statusRunning     = "running"
	statusLimitPaused = "limit_paused"
	statusHeld        = "held"
	statusDone        = "done"
	statusFailed      = "failed"
	statusCanceled    = "canceled"
)

type Task struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Type     string   `json:"type"`
	Priority int      `json:"priority"`
	Status   string   `json:"status"`
	Dir      string   `json:"dir"`
	Prompts  []string `json:"prompts"`
	Step     int      `json:"step"`

	SessionID string `json:"session_id,omitempty"`
	// Model 非空时以 --model 传给 claude（如 haiku/sonnet/opus），用于按任务难度路由模型。
	Model string `json:"model,omitempty"`
	// Runner 记录实际执行器："codex" 表示由备用 codex CLI 执行（claude 冷却期切换）。
	Runner string `json:"runner,omitempty"`
	// PreferRunner 为 "codex" 时任务钉在 codex 上执行（不管 claude 忙闲），
	// 用独立的 GPT 额度跑填充类任务；要求任务满足 codexEligible（fresh 或单步无会话）。
	PreferRunner string `json:"runner_pref,omitempty"`
	// RemoteHost 非空时任务在该远程主机执行（SSH → 远端 codex），键入 Config.RemoteHosts。
	// 让 5090 等机器进编排（跨机 dev，如 Trading）；要求 remoteEligible（单步/fresh、无 claude 会话）。
	RemoteHost string `json:"remote_host,omitempty"`
	// MidStep 表示当前步骤执行到一半被限额打断：恢复时发送续跑提示而不是重发原 prompt。
	MidStep bool `json:"mid_step,omitempty"`

	EmitTasks   bool `json:"emit_tasks,omitempty"`
	ReviewAfter bool `json:"review_after,omitempty"`
	// Effort 非空时以 --effort 传给 claude（low/medium/high/xhigh/max），按任务难度调思考等级。
	Effort string `json:"effort,omitempty"`
	// ReviewOf: 本卡是审核卡时指向被审卡 ID；修复闭环靠它取被审卡的 prompt/参数做继承。
	ReviewOf string `json:"review_of,omitempty"`
	// FixRound: "实现→对抗审核→修复"循环轮次。实现卡 0，第 n 轮自动修复卡为 n；审核卡继承被审卡轮次。
	// 达到 config.max_fix_rounds 后不再自动派修复，改挂 held 升级卡交人工/设计权威裁定。
	FixRound int `json:"fix_round,omitempty"`
	// FreshSteps 表示步骤间不 --resume：每一步都是全新会话（配合"状态在文件里"的项目规约，
	// prompt 自带读状态文件的开工动作）。永不依赖会话记忆，也就不会撞会话上下文上限。
	FreshSteps bool `json:"fresh_steps,omitempty"`
	// EmitHold 表示本任务产出的任务先挂起（held），人工审核后 release 放行。
	EmitHold bool `json:"emit_hold,omitempty"`
	// EmitProgress 表示任务完成后把最终输出中的 json 块存为进度报告（progress/ 目录）。
	EmitProgress bool `json:"emit_progress,omitempty"`
	// ProgressKey 是进度报告的落盘键；EmitProgress 时使用，空则用任务 ID。
	ProgressKey string `json:"progress_key,omitempty"`

	ResumeAtEpoch  int64 `json:"resume_at_epoch,omitempty"`
	NotBeforeEpoch int64 `json:"not_before_epoch,omitempty"`
	Attempts       int   `json:"attempts,omitempty"`

	PermissionMode  string   `json:"permission_mode,omitempty"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	SkipPermissions bool     `json:"skip_permissions,omitempty"`

	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	LastError string  `json:"last_error,omitempty"`
	TurnsUsed int     `json:"turns_used,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	// LastSummary 是最近一步执行输出的一行摘要，供 list 看板展示“最新进度概述”。
	LastSummary string `json:"last_summary,omitempty"`
}

func (t *Task) touch() { t.UpdatedAt = time.Now().Format(time.RFC3339) }

func (t *Task) terminal() bool {
	return t.Status == statusDone || t.Status == statusFailed || t.Status == statusCanceled
}

func newID(root string) string {
	for {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		id := fmt.Sprintf("t%s-%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
		if _, err := os.Stat(filepath.Join(tasksDir(root), id+".json")); os.IsNotExist(err) {
			return id
		}
	}
}

// newTask 创建任务并把类型默认参数烘焙进去。
func newTask(root string, cfg *Config, typ, title, dir string, prompts []string, priority int) *Task {
	now := time.Now().Format(time.RFC3339)
	t := &Task{
		ID:        newID(root),
		Title:     title,
		Type:      typ,
		Priority:  priority,
		Status:    statusQueued,
		Dir:       dir,
		Prompts:   prompts,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if td, ok := cfg.TypeDefaults[typ]; ok {
		t.PermissionMode = td.PermissionMode
		t.AllowedTools = append([]string(nil), td.AllowedTools...)
		t.SkipPermissions = td.SkipPermissions
		t.Model = td.Model
		t.Effort = td.Effort
	}
	return t
}

// findTaskAnywhere 先在 tasks/ 再在 archive/ 按精确 ID 找任务
// （修复闭环取谱系时，被审卡可能已被 clean 归档）。
func findTaskAnywhere(root, id string) (*Task, error) {
	for _, dir := range []string{tasksDir(root), archiveDir(root)} {
		p := filepath.Join(dir, id+".json")
		if data, err := os.ReadFile(p); err == nil {
			var t Task
			if err := json.Unmarshal(data, &t); err != nil {
				return nil, err
			}
			return &t, nil
		}
	}
	return nil, fmt.Errorf("任务 %s 不存在（tasks/ 与 archive/ 均无）", id)
}

func taskPath(root, id string) string { return filepath.Join(tasksDir(root), id+".json") }

func saveTask(root string, t *Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(taskPath(root, t.ID), append(data, '\n'))
}

func loadTask(root, id string) (*Task, error) {
	data, err := os.ReadFile(taskPath(root, id))
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("解析任务 %s 失败: %w", id, err)
	}
	return &t, nil
}

func loadTasks(root string) ([]*Task, error) {
	entries, err := os.ReadDir(tasksDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到 %s，请先运行: claudego init", tasksDir(root))
		}
		return nil, err
	}
	var tasks []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := loadTask(root, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			// ReadDir 与逐个读文件之间任务可能被并发归档移走（如 cancel 收尾），不是损坏，静默跳过。
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "警告: 跳过损坏的任务文件 %s: %v\n", e.Name(), err)
			}
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	return tasks, nil
}

// findTask 支持 ID 前缀匹配。
func findTask(root, prefix string) (*Task, error) {
	tasks, err := loadTasks(root)
	if err != nil {
		return nil, err
	}
	var matches []*Task
	for _, t := range tasks {
		if t.ID == prefix {
			return t, nil
		}
		if strings.HasPrefix(t.ID, prefix) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("找不到任务: %s", prefix)
	case 1:
		return matches[0], nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("前缀 %q 匹配到多个任务: %s", prefix, strings.Join(ids, ", "))
	}
}

// diskCanceled 判断任务在盘上是否已被取消。cancel 命令与调度进程各跑各的，
// 只能靠任务文件表态：状态为 canceled，或文件已不在 tasks/（非运行态 cancel
// 直接归档移走、或被人工删除）都算取消。读不出的损坏文件不算（别误杀）。
func diskCanceled(root, id string) bool {
	t, err := loadTask(root, id)
	if err != nil {
		return os.IsNotExist(err)
	}
	return t.Status == statusCanceled
}

func archiveTask(root string, t *Task) error {
	if err := os.MkdirAll(archiveDir(root), 0o755); err != nil {
		return err
	}
	return os.Rename(taskPath(root, t.ID), filepath.Join(archiveDir(root), t.ID+".json"))
}
