package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type usageInfo struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type claudeResult struct {
	Type         string     `json:"type"`
	Subtype      string     `json:"subtype"`
	IsError      bool       `json:"is_error"`
	Result       string     `json:"result"`
	SessionID    string     `json:"session_id"`
	NumTurns     int        `json:"num_turns"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	DurationMS   int64      `json:"duration_ms"`
	Usage        *usageInfo `json:"usage"`
}

var (
	// 已知的限额提示形态：
	//   "Claude AI usage limit reached|1751600000"（headless 常见，带重置的 unix 时间戳）
	//   "You've reached your usage limit ... resets at 3pm"
	//   "5-hour limit reached"
	limitRe     = regexp.MustCompile(`(?i)usage limit|limit reached|out of extra usage|hit your limit|limit will reset`)
	epochRe     = regexp.MustCompile(`\|\s*(\d{9,11})`)
	resetTimeRe = regexp.MustCompile(`(?i)reset[s]?\s*(?:at\s*)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	transientRe = regexp.MustCompile(`(?i)rate.?limit|overloaded|internal server error|api error 5\d\d|econnre|network error|timed? ?out`)
	fencedRe    = regexp.MustCompile("(?s)```json\\s*(.*?)```")
)

func invokeClaude(cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	args := []string{"-p", "--output-format", "json"}
	if t.SessionID != "" {
		args = append(args, "--resume", t.SessionID)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	} else if t.PermissionMode != "" {
		args = append(args, "--permission-mode", t.PermissionMode)
	}
	if len(t.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(t.AllowedTools, ","))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.ClaudeBin, args...)
	cmd.Dir = t.Dir
	if cfg.ThinkingTokens > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprintf("MAX_THINKING_TOKENS=%d", cfg.ThinkingTokens))
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseClaudeJSON(stdout.String())
	return res, combined, runErr
}

// invokeCodex 用 codex exec 执行一步（备用执行器）。结果通过 --output-last-message 取回，
// 包装成 claudeResult 以复用后续的 emit/进度解析管线。
func invokeCodex(cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	sandbox := "read-only"
	var extra []string
	if t.Type == typeSequence {
		sandbox = "workspace-write"
		// codex 沙箱默认禁写 .git，导致收工 commit 失败（活干了提交不了）；显式放行本仓 .git。
		extra = []string{"-c", fmt.Sprintf(`sandbox_workspace_write.writable_roots=["%s"]`, filepath.Join(t.Dir, ".git"))}
	}
	outFile := filepath.Join(os.TempDir(), "claudego-codex-"+t.ID+".txt")
	defer os.Remove(outFile)
	args := []string{"exec", "-C", t.Dir, "--sandbox", sandbox, "--skip-git-repo-check",
		"--color", "never", "-o", outFile}
	args = append(args, extra...)
	if cfg.CodexModel != "" {
		args = append(args, "-m", cfg.CodexModel)
	}
	if cfg.CodexReasoning != "" {
		args = append(args, "-c", "model_reasoning_effort="+cfg.CodexReasoning)
	}
	args = append(args, prompt)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.CodexBin, args...)
	cmd.Dir = t.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	last, _ := os.ReadFile(outFile)
	res := &claudeResult{Type: "result", Result: strings.TrimSpace(string(last))}
	if runErr != nil || res.Result == "" {
		res.IsError = true
		res.Subtype = "codex_error"
		if res.Result == "" {
			res.Result = firstLine(combined)
		}
	}
	return res, combined, runErr
}

// invokeRemoteCodex 通过 SSH 在远程主机上跑 codex exec（让 5090 等机器进编排）。
// prompt 走 ssh stdin 灌进 codex（codex exec 无 prompt 参数时读 stdin），彻底绕开 Windows cmd 引号；
// 结果由远端 codex 写到 -o 文件，再用 marker + type/cat 回捕到 stdout，隔开 codex 的执行日志噪声。
// 远端 codex 走自己的 GPT 额度：不记 claude 账本、不写全局冷却。安全靠 prompt 护栏 + 人工审 diff。
func invokeRemoteCodex(cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	rh, ok := cfg.RemoteHosts[t.RemoteHost]
	if !ok {
		return &claudeResult{Type: "result", IsError: true, Subtype: "remote_config"}, "",
			fmt.Errorf("未配置远程主机 %q（config.remote_hosts）", t.RemoteHost)
	}
	sshBin := cfg.SSHBin
	if sshBin == "" {
		sshBin = "ssh"
	}
	codexBin := rh.CodexBin
	if codexBin == "" {
		codexBin = "codex"
	}
	sandbox := rh.Sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	tmp := rh.TmpDir
	if tmp == "" {
		tmp = "."
	}
	outFile := tmp + "/claudego-remote-" + t.ID + ".txt"

	const marker = "===CLAUDEGO_REMOTE_RESULT==="
	// 远端 shell 差异：cmd（Windows，默认）用 & 分隔 + type + 反斜杠路径；posix 用 ; + cat + 正斜杠。
	sep, catCmd, printPath := "&", "type", strings.ReplaceAll(outFile, "/", `\`)
	if rh.Shell == "posix" {
		sep, catCmd, printPath = ";", "cat", outFile
	}
	// codex -C / -o 用正斜杠（codex 自会规范化写盘）；结果打印用 shell 对应的路径分隔符。
	remoteCmd := fmt.Sprintf(`%s exec -C "%s" --sandbox %s --skip-git-repo-check --color never -o "%s"`,
		codexBin, t.Dir, sandbox, outFile)
	if rh.Reasoning != "" {
		remoteCmd += " -c model_reasoning_effort=" + rh.Reasoning
	}
	if cfg.CodexModel != "" {
		remoteCmd += " -m " + cfg.CodexModel
	}
	remoteCmd += fmt.Sprintf(` %s echo %s %s %s "%s"`, sep, marker, sep, catCmd, printPath)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBin, "-o", "BatchMode=yes", t.RemoteHost, remoteCmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("远程步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()

	// marker 之后的 stdout 即结果文件内容（LastIndex 隔开 codex exec 的执行日志噪声）。
	result := ""
	if idx := strings.LastIndex(stdout.String(), marker); idx >= 0 {
		result = strings.TrimSpace(stdout.String()[idx+len(marker):])
	}
	res := &claudeResult{Type: "result", Result: result}
	// 拿到 -o 结果文件内容即视为成功：Windows codex exec 常因非致命告警（model refresh 超时等）
	// 退出非零，退出码不足为凭；成功与否由是否产出终稿 + 人工审 diff 判定。
	if result != "" {
		return res, combined, nil
	}
	res.IsError = true
	res.Subtype = "remote_codex_error"
	res.Result = firstLine(combined)
	if runErr == nil {
		runErr = fmt.Errorf("远端 codex 无结果输出（marker 后为空）")
	}
	return res, combined, runErr
}

// codexEligible 判断任务能否交给备用执行器：没有 claude 会话要延续即可——
// 单步未开跑的任务，以及 fresh_steps 任务的任意一步（状态在文件里，谁来跑都一样）。
func codexEligible(t *Task) bool {
	if t.SessionID != "" || t.MidStep {
		return false
	}
	return t.FreshSteps || (len(t.Prompts) == 1 && t.Step == 0)
}

func parseClaudeJSON(out string) *claudeResult {
	var res claudeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err == nil && res.Type != "" {
		return &res
	}
	// 输出前可能混入了非 JSON 行，逐行找 result 对象
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"type"`) {
			continue
		}
		var r claudeResult
		if err := json.Unmarshal([]byte(line), &r); err == nil && r.Type == "result" {
			return &r
		}
	}
	return nil
}

func isLimitHit(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError {
		return false
	}
	text := combined
	if res != nil {
		text += "\n" + res.Result
	}
	return limitRe.MatchString(text)
}

// parseResetEpoch 从错误输出中解析限额重置时间；解析不到则用配置的回退等待。
func parseResetEpoch(text string, cfg *Config, now time.Time) int64 {
	margin := int64(cfg.CooldownMarginSec)
	if m := epochRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if v > now.Unix()-600 && v < now.Add(26*time.Hour).Unix() {
				return clampEpoch(v+margin, now)
			}
		}
	}
	if m := resetTimeRe.FindStringSubmatch(text); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		switch strings.ToLower(m[3]) {
		case "pm":
			if hour < 12 {
				hour += 12
			}
		case "am":
			if hour == 12 {
				hour = 0
			}
		}
		if hour < 24 {
			cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
			if !cand.After(now) {
				cand = cand.Add(24 * time.Hour)
			}
			return clampEpoch(cand.Unix()+margin, now)
		}
	}
	return now.Add(time.Duration(cfg.LimitFallbackMin)*time.Minute).Unix() + margin
}

func clampEpoch(v int64, now time.Time) int64 {
	if min := now.Unix() + 120; v < min {
		return min
	}
	return v
}

// runTask 在一次派发内循环执行任务的各个步骤，直到完成、失败或撞上限额。
// 同一任务的多个步骤通过 --resume 复用同一会话，保持上下文连续。
// useCodex 为 true 时用备用执行器 codex exec（限单步任务，冷却/红线期间由 tick 决定）。
func runTask(root string, cfg *Config, t *Task, useCodex bool) error {
	t.Status = statusRunning
	remote := t.RemoteHost != ""
	switch {
	case remote:
		t.Runner = "remote:" + t.RemoteHost
	case useCodex:
		t.Runner = "codex"
	default:
		t.Runner = "" // 清除历史执行器标签（如降级失败后的重试回到 claude）
	}
	t.touch()
	if err := saveTask(root, t); err != nil {
		return err
	}
	lg, err := openTaskLog(root, t.ID)
	if err != nil {
		return err
	}
	defer lg.Close()

	for {
		now := time.Now()
		// 多步任务在步骤之间复查红线：越线则回到排队（会话与进度保留），等窗口滑走。
		if t.Step > 0 && !t.MidStep {
			if blocked, reason := budgetBlocked(root, cfg, now); blocked {
				t.Status = statusQueued
				t.NotBeforeEpoch = now.Add(5 * time.Minute).Unix()
				t.LastError = "额度红线: " + reason
				t.touch()
				logBlock(lg, "BUDGET", reason)
				return saveTask(root, t)
			}
		}
		var prompt string
		resuming := t.MidStep && t.SessionID != ""
		switch {
		case resuming:
			prompt = cfg.ResumePrompt
		case t.Step < len(t.Prompts):
			prompt = injectLiveContext(root, t.ID, t.Prompts[t.Step])
		default:
			t.Status = statusDone
			t.touch()
			return saveTask(root, t)
		}

		logSection(lg, fmt.Sprintf("步骤 %d/%d%s  session=%s%s", t.Step+1, len(t.Prompts),
			map[bool]string{true: "（限额中断后续跑）", false: ""}[resuming], orDash(t.SessionID),
			map[bool]string{true: "  runner=codex", false: ""}[useCodex]))
		logBlock(lg, "PROMPT", prompt)

		var res *claudeResult
		var combined string
		var runErr error
		switch {
		case remote:
			// 远端 codex 走自己的 GPT 额度：不记 claude 账本、不写全局冷却；错误按普通失败退避。
			res, combined, runErr = invokeRemoteCodex(cfg, t, prompt)
		case useCodex:
			// codex 走自己的额度：不记 claude 账本；其限额/错误按普通错误退避，不写全局冷却。
			res, combined, runErr = invokeCodex(cfg, t, prompt)
		default:
			res, combined, runErr = invokeClaude(cfg, t, prompt)
			if res != nil && res.SessionID != "" {
				t.SessionID = res.SessionID
			}
			if res != nil {
				appendUsage(root, cfg, t, res.Usage)
			}
		}

		// 1) 限额：记录恢复时间，全局冷却，等 tick 到点自动续跑
		if !useCodex && !remote && isLimitHit(res, combined) {
			until := parseResetEpoch(combined+"\n"+resultText(res), cfg, now)
			setCooldown(root, until, firstLine(combined))
			t.Status = statusLimitPaused
			t.ResumeAtEpoch = until
			t.MidStep = t.SessionID != ""
			if t.FreshSteps {
				// 状态在文件里：恢复时重发本步 prompt 开新会话即可，不需要续跑提示。
				t.SessionID = ""
				t.MidStep = false
			}
			t.LastError = "usage limit: " + firstLine(combined)
			t.touch()
			logBlock(lg, "LIMIT", fmt.Sprintf("命中用量限额，%s 后恢复（%s）\n%s", fmtIn(until, now), fmtClock(until), firstLine(combined)))
			return saveTask(root, t)
		}

		// 2) 其他失败：退避重试，超过次数则失败
		if runErr != nil || res == nil || res.IsError {
			msg := errorSummary(res, combined, runErr)
			t.Attempts++
			t.LastError = msg
			logBlock(lg, "ERROR", fmt.Sprintf("第 %d 次失败: %s", t.Attempts, msg))
			if t.Attempts >= cfg.MaxAttempts {
				t.Status = statusFailed
			} else {
				t.Status = statusQueued
				backoff := time.Duration(cfg.RetryBackoffMin) * time.Minute
				if transientRe.MatchString(msg) {
					backoff = time.Duration(cfg.RetryBackoffMin) * time.Minute
				} else {
					backoff *= time.Duration(t.Attempts)
				}
				t.NotBeforeEpoch = now.Add(backoff).Unix()
			}
			t.touch()
			return saveTask(root, t)
		}

		// 3) 成功：推进步骤（codex/远端成功不代表 claude 限额解除，冷却只由 claude 路径清除）
		if !useCodex && !remote {
			clearCooldown(root)
		}
		t.Attempts = 0
		t.NotBeforeEpoch = 0
		t.MidStep = false
		t.Step++
		if t.FreshSteps {
			t.SessionID = "" // 下一步全新会话
		}
		t.TurnsUsed += res.NumTurns
		t.CostUSD += res.TotalCostUSD
		t.LastError = ""
		if s := summarizeResult(res.Result); s != "" {
			t.LastSummary = s
		}
		logBlock(lg, "RESULT", res.Result)
		logSection(lg, fmt.Sprintf("步骤完成  turns=%d cost=$%.4f duration=%.0fs", res.NumTurns, res.TotalCostUSD, float64(res.DurationMS)/1000))

		if t.Step >= len(t.Prompts) {
			t.Status = statusDone
			t.touch()
			if err := saveTask(root, t); err != nil {
				return err
			}
			postComplete(root, cfg, t, res, lg)
			return saveTask(root, t)
		}
		t.touch()
		if err := saveTask(root, t); err != nil {
			return err
		}
	}
}

// postComplete 处理任务链：进度报告落盘；装配/协调任务产出的新任务入队；review_after 自动入队设计审核。
func postComplete(root string, cfg *Config, t *Task, res *claudeResult, lg *os.File) {
	if t.EmitProgress {
		if key, err := saveProgressFromResult(root, t, res.Result); err != nil {
			t.LastError = "进度报告落盘失败: " + err.Error()
			logBlock(lg, "PROGRESS", t.LastError)
		} else {
			logBlock(lg, "PROGRESS", "进度报告已写入: "+progressPath(root, key))
		}
	}
	if t.EmitTasks {
		created, err := enqueueEmitted(root, cfg, t, res.Result)
		if err != nil {
			t.LastError = "解析产出任务失败: " + err.Error()
			// 把原始 json 块存盘：模型常犯"字符串内未转义引号"这类小错，留着人工修复后补投。
			dump := filepath.Join(logsDir(root), t.ID+".emit-failed.json")
			if raw := lastFencedJSON(res.Result); raw != "" {
				_ = os.WriteFile(dump, []byte(raw+"\n"), 0o644)
				logBlock(lg, "EMIT", t.LastError+"\n原始 JSON 已存: "+dump)
			} else {
				logBlock(lg, "EMIT", t.LastError)
			}
		} else {
			logBlock(lg, "EMIT", "已入队: "+strings.Join(created, ", "))
		}
	}
	if t.ReviewAfter {
		tpl, err := loadTemplate(root, typeReview)
		if err == nil {
			focus := fmt.Sprintf("审查任务「%s」刚刚在该目录产生的改动（可结合 git diff/log）", t.Title)
			prompt := renderTemplate(tpl, map[string]string{"DIR": t.Dir, "FOCUS": focus})
			rv := newTask(root, cfg, typeReview, "审核: "+t.Title, t.Dir, []string{prompt}, t.Priority)
			if saveTask(root, rv) == nil {
				logBlock(lg, "REVIEW", "已入队审核任务: "+rv.ID)
			}
		}
	}
}

type emitSpec struct {
	Tasks []struct {
		Title       string   `json:"title"`
		Type        string   `json:"type"`
		Dir         string   `json:"dir"`
		Priority    int      `json:"priority"`
		Model       string   `json:"model"`
		SessionID   string   `json:"session_id"`
		ReviewAfter bool     `json:"review_after"`
		FreshSteps  bool     `json:"fresh_steps"`
		Runner      string   `json:"runner"`
		Prompts     []string `json:"prompts"`
		// 模型常见的字段名漂移，做别名容错：steps=[...] / prompt="..."
		Steps  []string `json:"steps"`
		Prompt string   `json:"prompt"`
	} `json:"tasks"`
}

func enqueueEmitted(root string, cfg *Config, parent *Task, result string) ([]string, error) {
	raw := lastFencedJSON(result)
	if raw == "" {
		raw = strings.TrimSpace(result)
	}
	var spec emitSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("输出中没有合法的 tasks JSON: %w", err)
	}
	if len(spec.Tasks) == 0 {
		return nil, fmt.Errorf("tasks 列表为空")
	}
	if len(spec.Tasks) > 10 {
		spec.Tasks = spec.Tasks[:10]
	}
	var ids []string
	for _, s := range spec.Tasks {
		if len(s.Prompts) == 0 && len(s.Steps) > 0 {
			s.Prompts = s.Steps
		}
		if len(s.Prompts) == 0 && strings.TrimSpace(s.Prompt) != "" {
			s.Prompts = []string{s.Prompt}
		}
		if len(s.Prompts) == 0 {
			continue
		}
		typ := s.Type
		// 模型有时自造类型名（如 "batch"）；未知类型会导致 newTask 不烘焙 typeDefaults
		// → 空权限裸跑被拒。未知或空一律回退 sequence，保证权限正确烘焙。
		if !validTypes[typ] {
			typ = typeSequence
		}
		dir := s.Dir
		if dir == "" {
			dir = parent.Dir
		}
		title := s.Title
		if title == "" {
			title = "由 " + parent.ID + " 生成"
		}
		nt := newTask(root, cfg, typ, title, dir, s.Prompts, s.Priority)
		nt.ReviewAfter = s.ReviewAfter
		// 协调链：产出的 coordinate 任务同样具备 emit 能力（自愈式续排——每批收尾排下一批）。
		// 递归有界：每次 emit ≤10 张、每张协调都消耗额度且受红线节流，模板负责终止条款。
		if typ == typeCoordinate {
			nt.EmitTasks = true
		}
		nt.FreshSteps = s.FreshSteps
		// 协调可把填充类任务钉在 codex 上（独立 GPT 额度）；形状不合规则忽略指定。
		if s.Runner == "codex" {
			nt.PreferRunner = "codex"
			if !codexEligible(nt) {
				nt.PreferRunner = ""
			}
		}
		if s.Model != "" {
			nt.Model = s.Model
		}
		nt.SessionID = s.SessionID
		if parent.EmitHold {
			nt.Status = statusHeld
		}
		if err := saveTask(root, nt); err != nil {
			return ids, err
		}
		ids = append(ids, nt.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("所有产出任务都缺少 prompts")
	}
	return ids, nil
}

func lastFencedJSON(text string) string {
	ms := fencedRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return ""
	}
	return strings.TrimSpace(ms[len(ms)-1][1])
}

func resultText(res *claudeResult) string {
	if res == nil {
		return ""
	}
	return res.Result
}

func errorSummary(res *claudeResult, combined string, runErr error) string {
	if res != nil && res.IsError {
		s := res.Subtype
		if r := firstLine(res.Result); r != "" {
			s += ": " + r
		}
		return s
	}
	if runErr != nil {
		return runErr.Error() + " | " + firstLine(combined)
	}
	return "无法解析 claude 输出 | " + firstLine(combined)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 300 {
				return line[:300]
			}
			return line
		}
	}
	return ""
}

// summarizeResult 取一步最终输出里第一条实质内容行，作为 list 看板“最新进度概述”的回落来源。
// 跳过空行/代码围栏，剥掉常见 markdown 前缀，rune 安全截到 80。
func summarizeResult(result string) string {
	for _, ln := range strings.Split(result, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "```") {
			continue
		}
		ln = strings.TrimSpace(strings.TrimLeft(ln, "#*->・•· \t"))
		if len([]rune(ln)) >= 4 {
			if r := []rune(ln); len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return ln
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func openTaskLog(root, id string) (*os.File, error) {
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(taskLogPath(root, id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

func logSection(f *os.File, s string) {
	fmt.Fprintf(f, "\n===== %s  %s =====\n", time.Now().Format("2006-01-02 15:04:05"), s)
}

func logBlock(f *os.File, label, body string) {
	fmt.Fprintf(f, "--- %s ---\n%s\n", label, strings.TrimSpace(body))
}
