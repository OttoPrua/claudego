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
	limitRe     = regexp.MustCompile(`(?i)usage limit|limit reached|out of extra usage|hit your limit|limit will reset|out of usage credits|out of credits|/usage-credits`)
	epochRe     = regexp.MustCompile(`\|\s*(\d{9,11})`)
	resetTimeRe = regexp.MustCompile(`(?i)reset[s]?\s*(?:at\s*)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	transientRe = regexp.MustCompile(`(?i)rate.?limit|overloaded|internal server error|api error 5\d\d|econnre|network error|timed? ?out`)
	fencedRe    = regexp.MustCompile("(?s)```json\\s*(.*?)```")
	// emit 容错阶梯用：任意语言标签的围栏（模型常漏写 json 标签）与输出中提到的 .json 文件名。
	anyFencedRe = regexp.MustCompile("(?s)```[a-zA-Z]*[ \t]*\\n?(.*?)```")
	jsonFileRe  = regexp.MustCompile(`[\w./\\-]+\.json`)
)

func invokeClaude(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
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

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.ClaudeBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	if cfg.ThinkingTokens > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprintf("MAX_THINKING_TOKENS=%d", cfg.ThinkingTokens))
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := runCmdRegistered(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseClaudeJSON(stdout.String())
	return res, combined, runErr
}

// invokeCodex 用 codex exec 执行一步（备用执行器）。结果通过 --output-last-message 取回，
// 包装成 claudeResult 以复用后续的 emit/进度解析管线。
func invokeCodex(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
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

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.CodexBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegistered(cmd)
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

// invokeRemoteClaude 通过 SSH 在远程主机上跑 claude -p（远程 fable 设计等，用该主机自己的 claude 账号）。
// prompt 走 ssh stdin；claude -p --output-format json 直接把结果 JSON 打到 stdout（无需 marker/文件，复用 parseClaudeJSON）。
// claude -p 无 -C 参数，故先 cd 到工作目录（cmd 用 cd /d + 反斜杠，posix 用 cd + 正斜杠）。
func invokeRemoteClaude(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	rh, ok := cfg.RemoteHosts[t.RemoteHost]
	if !ok {
		return &claudeResult{Type: "result", IsError: true, Subtype: "remote_config"}, "",
			fmt.Errorf("未配置远程主机 %q（config.remote_hosts）", t.RemoteHost)
	}
	sshBin := cfg.SSHBin
	if sshBin == "" {
		sshBin = "ssh"
	}
	claudeBin := rh.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	cdCmd, dir := "cd /d", strings.ReplaceAll(t.Dir, "/", `\`)
	if rh.Shell == "posix" {
		cdCmd, dir = "cd", t.Dir
	}
	args := claudeBin + " -p --output-format json"
	if t.Model != "" {
		args += " --model " + t.Model
	}
	if t.SkipPermissions {
		args += " --dangerously-skip-permissions"
	} else if t.PermissionMode != "" {
		args += " --permission-mode " + t.PermissionMode
	}
	// allowedTools 清单含空格（如 "Bash(python3 -m pytest:*)"），拼进远程 shell 串必须整体加引号，
	// 否则 cmd/posix 都会按空格劈开、碎片被 claude 当独立参数（实测炸出 unknown option '-m'）。
	// skip-permissions 下清单本就无效，干脆不传，少一段引号地狱。
	if len(t.AllowedTools) > 0 && !t.SkipPermissions {
		args += ` --allowedTools "` + strings.Join(t.AllowedTools, ",") + `"`
	}
	remoteCmd := fmt.Sprintf(`%s "%s" && %s`, cdCmd, dir, args)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBin, "-o", "BatchMode=yes", t.RemoteHost, remoteCmd)
	setupProcGroup(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegistered(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("远程步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseClaudeJSON(stdout.String())
	if res == nil {
		res = &claudeResult{Type: "result", IsError: true, Subtype: "remote_claude_error", Result: firstLine(combined)}
	} else if runErr != nil && !res.IsError {
		// 远端 claude 已把完整结果 JSON 打到 stdout,但它派生的后台子进程（如探针脚本）
		// 可能吊着 ssh 管道不放,cmd.Run 只能等到超时才返回——结果在手即成功,
		// 超时/非零退出只是收尾竞态（同 invokeRemoteCodex 的"有结果即成功"原则,实测 F2-R3 被误标 failed）。
		runErr = nil
	}
	return res, combined, runErr
}

// invokeRemoteCodex 通过 SSH 在远程主机上跑 codex exec（让 5090 等机器进编排）。
// prompt 走 ssh stdin 灌进 codex（codex exec 无 prompt 参数时读 stdin），彻底绕开 Windows cmd 引号；
// 结果由远端 codex 写到 -o 文件，再用 marker + type/cat 回捕到 stdout，隔开 codex 的执行日志噪声。
// 远端 codex 走自己的 GPT 额度：不记 claude 账本、不写全局冷却。安全靠 prompt 护栏 + 人工审 diff。
func invokeRemoteCodex(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
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

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBin, "-o", "BatchMode=yes", t.RemoteHost, remoteCmd)
	setupProcGroup(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegistered(cmd)
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

// runTask 在一次派发内循环执行任务的各个步骤，直到完成、失败、撞上限额或被取消。
// 同一任务的多个步骤通过 --resume 复用同一会话，保持上下文连续。
// useCodex 为 true 时用备用执行器 codex exec（限单步任务，冷却/红线期间由 tick 决定）。
// ctx 由 tick 持有：任务被 cancel 后 tick 对账发现即取消 ctx，整组击杀执行进程。
func runTask(ctx context.Context, root string, cfg *Config, t *Task, useCodex bool) error {
	// 守门：pickNext 之后、开跑之前任务可能刚被 cancel（非运行态 cancel 直接归档移走文件），
	// 别把已取消的任务写回 tasks/ 复活成 running。
	if diskCanceled(root, t.ID) {
		t.Status = statusCanceled
		_ = archiveTask(root, t)
		return nil
	}
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
			// 远端执行：带 claude 模型(如 fable)走远端 claude；否则走远端 codex。
			// 两者都走该远端主机自己的账号额度，不记本机 claude 账本、不写全局冷却。
			if t.Model != "" {
				res, combined, runErr = invokeRemoteClaude(ctx, cfg, t, prompt)
			} else {
				res, combined, runErr = invokeRemoteCodex(ctx, cfg, t, prompt)
			}
		case useCodex:
			// codex 走自己的额度：不记 claude 账本；其限额/错误按普通错误退避，不写全局冷却。
			res, combined, runErr = invokeCodex(ctx, cfg, t, prompt)
		default:
			res, combined, runErr = invokeClaude(ctx, cfg, t, prompt)
			if res != nil && res.SessionID != "" {
				t.SessionID = res.SessionID
			}
			if res != nil {
				appendUsage(root, cfg, t, res.Usage)
			}
		}

		// 0) 取消：tick 对账发现盘上已标 canceled 后取消 ctx 击杀进程组；也可能进程
		// 自然结束后才发现取消标记（cancel 落在步骤间隙）。两种都按取消收尾：
		// 产物丢弃（远端"结果在手即成功"的救援同样让位），不再把内存态写回任务文件。
		// 残余竞态窗口：cancel 恰落在本检查与本步 saveTask 之间的微秒级间隙会被盖掉，
		// 由 tick 的周期对账兜底不了（文件已非 canceled），接受——窗口从整步时长缩到微秒。
		if ctx.Err() != nil || diskCanceled(root, t.ID) {
			return finalizeCanceled(root, t, lg)
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

		// 1b) 远端撞该主机账号限额（如 5090 的 fable/GPT 账号）：按本任务 resume_at 挂起，
		// 不写全局冷却（远端账号与本机独立）；eligible() 到刷新时刻才再派 → 无损接力自动续跑。
		if remote && isLimitHit(res, combined) {
			until := parseResetEpoch(combined+"\n"+resultText(res), cfg, now)
			t.Status = statusLimitPaused
			t.ResumeAtEpoch = until
			t.MidStep = false
			t.SessionID = "" // 远端无会话可续，恢复时重发本步开新会话
			t.LastError = "远端账号限额: " + firstLine(resultText(res))
			t.touch()
			logBlock(lg, "LIMIT", fmt.Sprintf("远端账号限额，%s 后恢复（%s）", fmtIn(until, now), fmtClock(until)))
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
			// postComplete 期间任务可能被 cancel 并归档（done 态 cancel 走立即归档），
			// 注解回写别把归档移走的文件复活。
			if diskCanceled(root, t.ID) {
				return nil
			}
			return saveTask(root, t)
		}
		t.touch()
		if err := saveTask(root, t); err != nil {
			return err
		}
	}
}

// finalizeCanceled 按取消收尾：执行进程（组）已被击杀或已自然结束，本步产物丢弃，
// 归档盘上的任务文件——cancel 命令对 running 任务只写取消标记不归档（进程还活着），
// 归档由这里补上；文件已被移走（非运行态 cancel 或人工删除）则忽略。
func finalizeCanceled(root string, t *Task, lg *os.File) error {
	t.Status = statusCanceled
	logBlock(lg, "CANCELED", "任务已取消：终止执行进程，丢弃本步产物并归档。")
	if err := archiveTask(root, t); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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

type emitTask struct {
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
	// 模型常见的字段名漂移，做别名容错：steps=[...] / prompt="..." / 标题写成 role 或 id。
	Steps  []string `json:"steps"`
	Prompt string   `json:"prompt"`
	Role   string   `json:"role"`
	ID     string   `json:"id"`
}

type emitSpec struct {
	Tasks []emitTask `json:"tasks"`
}

// parseEmitTasks 尝试把一段文本按 {"tasks":[...]} 或裸数组 [...] 解析成任务清单。
// 解析不出或列表为空返回 nil。
func parseEmitTasks(raw string) []emitTask {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var spec emitSpec
	if err := json.Unmarshal([]byte(raw), &spec); err == nil && len(spec.Tasks) > 0 {
		return spec.Tasks
	}
	var arr []emitTask
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return arr
	}
	return nil
}

// extractEmitTasks 从模型输出里提取产出任务，容错阶梯（实战三次 emit 失败的教训——
// 模型会漏写 json 围栏标签、把 JSON 混在叙述里、甚至只把清单写进文件不回显）：
//  1. 围栏块（json 标签或裸围栏，内容以 {/[ 开头），后出现的优先；
//  2. 未围栏平衡扫描：在 "tasks" 关键字前回溯 '{'，用 Decoder 解出首个合法对象；
//  3. 文件救援：输出中提到的 .json 文件（限任务目录内），读文件解析。
func extractEmitTasks(result, dir string) ([]emitTask, error) {
	// ① 围栏块，后出现的优先（模型习惯把操作性 JSON 放最后）。
	ms := anyFencedRe.FindAllStringSubmatch(result, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		body := strings.TrimSpace(ms[i][1])
		if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
			continue
		}
		if tasks := parseEmitTasks(body); tasks != nil {
			return tasks, nil
		}
	}
	// ② 未围栏：定位每个 "tasks" 出现点，向前回溯若干 '{' 逐一试解
	// （最近的 '{' 可能落在嵌套对象或字符串内，逐层外推直到解出）。
	for off := 0; ; {
		k := strings.Index(result[off:], `"tasks"`)
		if k < 0 {
			break
		}
		pos := off + k
		brace := pos
		for attempt := 0; attempt < 32; attempt++ {
			brace = strings.LastIndex(result[:brace], "{")
			if brace < 0 {
				break
			}
			var spec emitSpec
			dec := json.NewDecoder(strings.NewReader(result[brace:]))
			if err := dec.Decode(&spec); err == nil && len(spec.Tasks) > 0 {
				return spec.Tasks, nil
			}
		}
		off = pos + len(`"tasks"`)
	}
	// ③ 文件救援：模型把清单写进了任务目录下的 .json 文件（如 _WAVE-N-TASKS.json）。
	if dir != "" {
		base := filepath.Clean(dir)
		seen := map[string]bool{}
		for _, tok := range jsonFileRe.FindAllString(result, -1) {
			p := strings.ReplaceAll(tok, "\\", "/")
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
			p = filepath.Clean(p)
			// 只信任务目录内的文件，拒绝越界路径。
			if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
				continue
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Size() > 2<<20 {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if tasks := parseEmitTasks(string(b)); tasks != nil {
				return tasks, nil
			}
		}
	}
	return nil, fmt.Errorf("输出中没有合法的 tasks JSON（围栏/平衡扫描/文件救援均未命中）")
}

func enqueueEmitted(root string, cfg *Config, parent *Task, result string) ([]string, error) {
	tasks, err := extractEmitTasks(result, parent.Dir)
	if err != nil {
		return nil, err
	}
	if len(tasks) > 10 {
		tasks = tasks[:10]
	}
	var ids []string
	for _, s := range tasks {
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
			title = s.Role
		}
		if title == "" {
			title = s.ID
		}
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
