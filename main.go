package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const version = "0.10.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "assemble":
		err = cmdAssemble(os.Args[2:])
	case "review":
		err = cmdReview(os.Args[2:])
	case "adopt":
		err = cmdAdopt(os.Args[2:])
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "brief":
		err = cmdBrief(os.Args[2:])
	case "progress":
		err = cmdProgress(os.Args[2:])
	case "sessions":
		err = cmdSessions(os.Args[2:])
	case "cmd":
		err = cmdCmd(os.Args[2:])
	case "quota":
		err = cmdQuota(os.Args[2:])
	case "list", "status", "ls":
		err = cmdList(os.Args[2:])
	case "run", "tick":
		err = cmdRun(os.Args[2:])
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "hold":
		err = cmdSetStatus(os.Args[2:], "hold")
	case "release":
		err = cmdSetStatus(os.Args[2:], "release")
	case "retry":
		err = cmdSetStatus(os.Args[2:], "retry")
	case "cancel":
		err = cmdSetStatus(os.Args[2:], "cancel")
	case "log":
		err = cmdLog(os.Args[2:])
	case "clean":
		err = cmdClean(os.Args[2:])
	case "install-launchd":
		err = cmdInstallLaunchd(os.Args[2:])
	case "uninstall-launchd":
		err = uninstallLaunchd()
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("claudego", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`claudego — 围绕 Claude 5 小时用量限额的本地任务队列

用法: claudego <命令> [选项]

添加任务
  add       [-type sequence|design-review|prompt-assembly|coordinate|progress-pull]
            [-title T] [-dir D] [-priority N] [-model haiku|sonnet|opus] [-file steps.md]
            [-review-after] [-emit] [-hold] [-skip-permissions] [-tools "A,B"] "prompt..."
            -file 中用单独一行 --- 分隔多个步骤（预设 prompt 序列）
  assemble  [-dir D] [-priority N] [-model M] [-session S] "目标描述"
            prompt 装配：调研后产出任务序列并自动入队；-session 挂到既有装配角色会话
  review    [-dir D] [-priority N] [-model M] [-session S] ["关注点"]
            设计审核：只读审查产出分级报告；-session 挂到既有审核角色会话
  adopt     <session-id> [-dir D] [-model M] ["续跑提示"]  # 接管一个被限额打断的既有会话

进度回收与分工协调
  sessions  [-dir D] [-n 10]              # 列出该项目最近的 claude 会话（桌面端与 CLI 同池）
  brief     [-id 任务ID | -session 会话ID] [-dir D] [-title T] [-auto]
            生成"整理当前进度"的 prompt（贴到进行中的会话，报告自动写回 progress/）
            -auto: 不用手贴，入队一个 haiku 回收任务 --resume 该会话自动取报告
  plan      [-dir D] [-priority N] "总体目标"    # 分工协调：按队列+进度报告拆分任务，
                                                # 产出各任务 prompt/模型建议并自动入队
  progress  [-show K | -rm K | -in [file] -key K]  # 查看/删除/手动导入进度报告
  cmd <id>                                     # 打印手动接管某任务的 claude 命令与 prompt

调度与执行
  run       [-force] [-quiet]      # 跑一轮：排空就绪队列，最多 max_parallel 路并行（同目录串行）
  daemon                           # 前台常驻轮询（不装 launchd 时用）
  list                             # 任务看板（-json 机器可读，-all 含已归档状态）
  log <id> [-n 60]                 # 查看任务执行日志
  quota                            # 5 小时额度视图：队列消耗/红线状态/外部用量源

任务管理
  hold/release <id>                # 挂起 / 恢复排队
  retry <id>                       # 失败任务重新入队（保留会话与进度）
  cancel <id>                      # 取消并归档
  clean                            # 把 done/failed/canceled 归档到 archive/

系统
  init                             # 初始化数据目录（默认 ~/.claudego，可用 CLAUDEGO_ROOT / -root 覆盖）
  install-launchd [-interval 300]  # 安装 macOS 定时器，开机自动调度
  uninstall-launchd
  doctor                           # 自检环境
`)
}

// ---- init ----

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)

	for _, d := range []string{root, tasksDir(root), archiveDir(root), logsDir(root)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := writeDefaultTemplates(root); err != nil {
		return err
	}
	if _, err := os.Stat(configPath(root)); os.IsNotExist(err) {
		bin := "claude"
		if abs, err := exec.LookPath("claude"); err == nil {
			bin = abs
		}
		if err := saveConfig(root, defaultConfig(bin)); err != nil {
			return err
		}
	}
	fmt.Printf(`初始化完成: %s

下一步:
  1. claudego add -title "..." -dir <项目目录> "你的 prompt"   # 或 assemble / review
  2. claudego run                                            # 手动跑一轮验证
  3. claudego install-launchd                                # 安装后台定时调度
配置: %s
`, root, configPath(root))
	return nil
}

// ---- add / assemble / review / adopt ----

var validTypes = map[string]bool{
	typeSequence: true, typeReview: true, typeAssembly: true,
	typeCoordinate: true, typeProgressPull: true,
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	typ := fs.String("type", typeSequence, "任务类型")
	title := fs.String("title", "", "任务标题")
	dir := fs.String("dir", "", "工作目录（默认当前目录）")
	priority := fs.Int("priority", 0, "优先级，越大越先跑")
	file := fs.String("file", "", "从文件读取 prompt，--- 行分隔步骤")
	reviewAfter := fs.Bool("review-after", false, "完成后自动入队设计审核")
	emit := fs.Bool("emit", false, "解析最终输出中的 tasks JSON 并入队（装配类任务）")
	hold := fs.Bool("hold", false, "先挂起，手动 release 后才参与调度")
	fresh := fs.Bool("fresh", false, "步骤间不复用会话：每步全新会话（状态放文件里的项目用）")
	session := fs.String("session", "", "从已有 claude 会话继续（--resume 该会话）")
	skipPerms := fs.Bool("skip-permissions", false, "以 --dangerously-skip-permissions 运行（完全自主，慎用）")
	tools := fs.String("tools", "", "覆盖允许的工具，逗号分隔")
	permMode := fs.String("permission-mode", "", "覆盖权限模式")
	model := fs.String("model", "", "覆盖模型（haiku/sonnet/opus 或完整模型名）")
	runner := fs.String("runner", "", "钉定执行器：codex = 走独立 GPT 额度（要求单步或 -fresh）")
	_ = fs.Parse(args)

	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if *runner != "" && *runner != "codex" {
		return fmt.Errorf("未知 runner %q（可选: codex）", *runner)
	}
	if !validTypes[*typ] {
		return fmt.Errorf("未知类型 %q（可选: %s, %s, %s, %s, %s）",
			*typ, typeSequence, typeReview, typeAssembly, typeCoordinate, typeProgressPull)
	}

	var prompts []string
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		prompts = splitSteps(string(data))
	} else if s := strings.TrimSpace(strings.Join(fs.Args(), " ")); s != "" {
		prompts = []string{s}
	}
	if len(prompts) == 0 {
		return fmt.Errorf("缺少 prompt：直接写在命令行，或用 -file 指定")
	}

	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	t := newTask(root, cfg, *typ, orDefaultTitle(*title, prompts[0]), wd, prompts, *priority)
	t.ReviewAfter = *reviewAfter
	t.EmitTasks = *emit || *typ == typeAssembly || *typ == typeCoordinate
	t.SessionID = *session
	if *hold {
		t.Status = statusHeld
	}
	if *skipPerms {
		t.SkipPermissions = true
	}
	if *tools != "" {
		t.AllowedTools = splitComma(*tools)
	}
	if *permMode != "" {
		t.PermissionMode = *permMode
	}
	if *model != "" {
		t.Model = *model
	}
	t.FreshSteps = *fresh
	if *runner == "codex" {
		t.PreferRunner = "codex"
		if !codexEligible(t) {
			return fmt.Errorf("-runner codex 要求任务单步无会话，或加 -fresh（状态在文件里，步骤间不续会话）")
		}
		if cfg.CodexBin == "" {
			return fmt.Errorf("config.json 未配置 codex_bin，无法钉定 codex 执行器")
		}
	}
	if err := saveTask(root, t); err != nil {
		return err
	}
	fmt.Printf("已入队 %s [%s] %s（%d 步，优先级 %d）\n", t.ID, t.Type, t.Title, len(t.Prompts), t.Priority)
	return nil
}

func cmdAssemble(args []string) error {
	fs := flag.NewFlagSet("assemble", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	dir := fs.String("dir", "", "目标项目目录（默认当前目录）")
	priority := fs.Int("priority", 0, "优先级")
	title := fs.String("title", "", "任务标题")
	model := fs.String("model", "", "覆盖模型")
	session := fs.String("session", "", "挂到既有装配会话上（--resume 续用其上下文）")
	holdOut := fs.Bool("hold", false, "产出的任务先挂起，人工审核后 release 放行")
	_ = fs.Parse(args)

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		return fmt.Errorf("用法: claudego assemble [-dir D] \"目标描述\"")
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	tpl, err := loadTemplate(root, typeAssembly)
	if err != nil {
		return err
	}
	prompt := renderTemplate(tpl, map[string]string{"GOAL": goal, "DIR": wd})
	t := newTask(root, cfg, typeAssembly, orDefaultTitle(*title, "装配: "+goal), wd, []string{prompt}, *priority)
	t.EmitTasks = true
	t.EmitHold = *holdOut
	t.SessionID = *session
	if *model != "" {
		t.Model = *model
	}
	if err := saveTask(root, t); err != nil {
		return err
	}
	fmt.Printf("已入队装配任务 %s：完成后产出的任务序列会自动进入队列。\n", t.ID)
	return nil
}

func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	dir := fs.String("dir", "", "目标项目目录（默认当前目录）")
	priority := fs.Int("priority", 0, "优先级")
	title := fs.String("title", "", "任务标题")
	model := fs.String("model", "", "覆盖模型")
	session := fs.String("session", "", "挂到既有审核会话上（--resume 续用其上下文）")
	_ = fs.Parse(args)

	focus := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if focus == "" {
		focus = "整体架构与近期改动"
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	tpl, err := loadTemplate(root, typeReview)
	if err != nil {
		return err
	}
	prompt := renderTemplate(tpl, map[string]string{"DIR": wd, "FOCUS": focus})
	t := newTask(root, cfg, typeReview, orDefaultTitle(*title, "审核: "+focus), wd, []string{prompt}, *priority)
	t.SessionID = *session
	if *model != "" {
		t.Model = *model
	}
	if err := saveTask(root, t); err != nil {
		return err
	}
	fmt.Printf("已入队审核任务 %s [%s]\n", t.ID, wd)
	return nil
}

func cmdAdopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	dir := fs.String("dir", "", "会话所属项目目录（默认当前目录）")
	priority := fs.Int("priority", 0, "优先级")
	model := fs.String("model", "", "覆盖模型")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("用法: claudego adopt <session-id> [-dir D] [\"续跑提示\"]")
	}
	sessionID := rest[0]
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(rest[1:], " "))
	if prompt == "" {
		prompt = cfg.ResumePrompt
	}
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	t := newTask(root, cfg, typeSequence, "接管会话 "+short, wd, []string{prompt}, *priority)
	t.SessionID = sessionID
	if *model != "" {
		t.Model = *model
	}
	if err := saveTask(root, t); err != nil {
		return err
	}
	fmt.Printf("已入队 %s：将 --resume %s 继续执行。\n", t.ID, sessionID)
	return nil
}

// ---- plan / brief / progress / cmd ----

// cmdPlan 入队一个分工协调任务：读实时队列与进度报告，把目标拆成带模型建议的任务并自动入队。
func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	dir := fs.String("dir", "", "主要工作目录（默认当前目录）")
	priority := fs.Int("priority", 6, "优先级（默认 6，先于常规任务）")
	title := fs.String("title", "", "任务标题")
	model := fs.String("model", "", "覆盖模型")
	holdOut := fs.Bool("hold", false, "分工产出的任务先挂起，人工审核后 release 放行")
	_ = fs.Parse(args)

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		return fmt.Errorf("用法: claudego plan [-dir D] \"总体目标\"")
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	tpl, err := loadTemplate(root, typeCoordinate)
	if err != nil {
		return err
	}
	// {{QUEUE}} / {{PROGRESS}} 不在此时渲染，留给派发时注入实时快照（injectLiveContext）。
	prompt := renderTemplate(tpl, map[string]string{"GOAL": goal, "DIR": wd})
	t := newTask(root, cfg, typeCoordinate, orDefaultTitle(*title, "分工: "+goal), wd, []string{prompt}, *priority)
	t.EmitTasks = true
	t.EmitHold = *holdOut
	applyLegacyTypeFallback(cfg, t)
	if *model != "" {
		t.Model = *model
	}
	if err := saveTask(root, t); err != nil {
		return err
	}
	if *holdOut {
		fmt.Printf("已入队协调任务 %s：分工产出的任务将挂起等待人工放行（claudego release）。\n", t.ID)
	} else {
		fmt.Printf("已入队协调任务 %s：运行时注入实时队列与进度报告，分工产出的任务自动入队。\n", t.ID)
	}
	fmt.Printf("分工说明（各任务的模型与手动接管命令）留在日志里: claudego log %s\n", t.ID)
	if ids := activeSessionTasks(root, t.ID); len(ids) > 0 {
		fmt.Printf("提示: %d 个未结束任务带有在途会话，可先回收进度让分工更准: claudego brief -id %s -auto\n", len(ids), ids[0])
	}
	return nil
}

// applyLegacyTypeFallback 兼容旧 config.json（type_defaults 里没有新类型）：套用内置默认，
// 避免协调/进度回收任务以空白权限运行。
func applyLegacyTypeFallback(cfg *Config, t *Task) {
	if _, ok := cfg.TypeDefaults[t.Type]; ok {
		return
	}
	if td, ok := defaultConfig(cfg.ClaudeBin).TypeDefaults[t.Type]; ok {
		t.PermissionMode = td.PermissionMode
		t.AllowedTools = append([]string(nil), td.AllowedTools...)
		t.SkipPermissions = td.SkipPermissions
		t.Model = td.Model
	}
}

func activeSessionTasks(root, excludeID string) []string {
	tasks, err := loadTasks(root)
	if err != nil {
		return nil
	}
	var ids []string
	for _, t := range tasks {
		if !t.terminal() && t.ID != excludeID && t.SessionID != "" {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// cmdBrief 回收某个会话的进度：默认打印"整理进度"的 prompt 供手贴（报告由会话写回 progress/），
// -auto 则入队一个便宜的回收任务 --resume 该会话自动取报告。
func cmdBrief(args []string) error {
	fs := flag.NewFlagSet("brief", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	id := fs.String("id", "", "目标任务 ID（用它的会话/目录/标题）")
	session := fs.String("session", "", "目标 claude 会话 ID")
	dir := fs.String("dir", "", "会话所属项目目录（默认当前目录）")
	title := fs.String("title", "", "进度条目标题")
	auto := fs.Bool("auto", false, "入队进度回收任务自动执行（需要会话 ID）")
	priority := fs.Int("priority", 8, "回收任务优先级（-auto 时）")
	model := fs.String("model", "", "覆盖回收任务模型（默认取 progress-pull 类型默认）")
	_ = fs.Parse(args)

	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}

	key, ttl, sess := "", *title, *session
	var wd string
	if *id != "" {
		t, err := findTask(root, *id)
		if err != nil {
			return err
		}
		key, wd = t.ID, t.Dir
		if sess == "" {
			sess = t.SessionID
		}
		if ttl == "" {
			ttl = t.Title
		}
	} else {
		wd, err = resolveDir(*dir)
		if err != nil {
			return err
		}
		if ttl == "" {
			ttl = filepath.Base(wd)
		}
		if sess != "" {
			short := sess
			if len(short) > 8 {
				short = short[:8]
			}
			key = "s-" + short
		} else {
			key = progressSlug(ttl)
		}
	}

	if *auto {
		if sess == "" {
			return fmt.Errorf("-auto 需要会话 ID：用 -id 指定带会话的任务，或用 -session 直接给会话 ID")
		}
		tpl, err := loadTemplate(root, typeProgressPull)
		if err != nil {
			return err
		}
		t := newTask(root, cfg, typeProgressPull, "进度: "+ttl, wd, []string{tpl}, *priority)
		t.SessionID = sess
		t.EmitProgress = true
		t.ProgressKey = key
		applyLegacyTypeFallback(cfg, t)
		if *model != "" {
			t.Model = *model
		}
		if err := saveTask(root, t); err != nil {
			return err
		}
		fmt.Printf("已入队进度回收任务 %s（--resume %s，模型 %s）：完成后报告写入 %s\n",
			t.ID, sess, orDash(t.Model), progressPath(root, key))
		return nil
	}

	// 手贴模式：prompt 里让会话自己把报告写到 progress/，实现"自动接收"。
	if err := os.MkdirAll(progressDir(root), 0o755); err != nil {
		return err
	}
	tpl, err := loadTemplate(root, "progress-brief")
	if err != nil {
		return err
	}
	prompt := renderTemplate(tpl, map[string]string{"OUT": progressPath(root, key), "KEY": key})
	fmt.Printf("把下面内容整段贴到目标会话里（报告会写入 %s，之后 claudego plan 直接可用）：\n\n", progressPath(root, key))
	fmt.Println(strings.TrimSpace(prompt))
	return nil
}

// cmdProgress 查看 / 手动导入 / 删除进度报告。
func cmdProgress(args []string) error {
	fs := flag.NewFlagSet("progress", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	in := fs.Bool("in", false, "从文件或标准输入导入进度 JSON（粘贴备用通道）")
	key := fs.String("key", "", "进度键（-in 时用；空则按标题生成）")
	title := fs.String("title", "", "进度条目标题（-in 时用）")
	dir := fs.String("dir", "", "关联目录（-in 时用）")
	session := fs.String("session", "", "关联会话（-in 时用）")
	show := fs.String("show", "", "显示某条进度（人读渲染）")
	full := fs.Bool("full", false, "-show 时展开 next_prompt 全文")
	rm := fs.String("rm", "", "删除某条进度")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)

	switch {
	case *rm != "":
		if err := os.Remove(progressPath(root, *rm)); err != nil {
			return err
		}
		fmt.Println("已删除进度:", *rm)
		return nil
	case *show != "":
		data, err := os.ReadFile(progressPath(root, *show))
		if err != nil {
			return err
		}
		var e ProgressEntry
		if err := json.Unmarshal(data, &e); err != nil || e.Report == nil {
			fmt.Print(string(data)) // 解析失败：退回原文，不丢信息
			return nil
		}
		renderReport(&e, *full)
		return nil
	case *in:
		var data []byte
		var err error
		if fs.NArg() > 0 {
			data, err = os.ReadFile(fs.Arg(0))
		} else {
			data, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) == "" {
			return fmt.Errorf("输入为空")
		}
		report := parseReportLoose(string(data))
		ttl := *title
		if ttl == "" {
			if g, ok := report["goal"].(string); ok {
				ttl = truncate(g, 48)
			}
		}
		k := *key
		if k == "" {
			k = progressSlug(ttl)
		}
		e := &ProgressEntry{Key: k, Title: ttl, Dir: *dir, SessionID: *session, Report: report}
		if err := saveProgress(root, e); err != nil {
			return err
		}
		fmt.Println("已导入进度:", progressPath(root, k))
		return nil
	}

	entries := loadProgressEntries(root)
	if len(entries) == 0 {
		fmt.Println("还没有进度报告。用 claudego brief 生成整理 prompt，或 brief -id <任务> -auto 自动回收。")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\t标题\t更新\t完成/剩/阻\t现状\t项目")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Key, truncate(e.Title, 20), shortTime(e.UpdatedAt),
			reportCounts(e.Report), truncate(reportStatus(e.Report), 44), filepath.Base(e.Dir))
	}
	w.Flush()
	fmt.Println("\n详情: claudego progress -show <KEY>；基于进度分工: claudego plan \"目标\"")
	return nil
}

func reportCounts(r map[string]any) string {
	if isRawReport(r) {
		return "raw"
	}
	c := func(k string) int {
		if v, ok := r[k].([]any); ok {
			return len(v)
		}
		return 0
	}
	return fmt.Sprintf("%d/%d/%d", c("done"), c("remaining"), c("blockers"))
}

func isRawReport(r map[string]any) bool {
	f, _ := r["format"].(string)
	return f == "raw"
}

func rptStr(v any) string { s, _ := v.(string); return s }
func rptArr(v any) []any  { a, _ := v.([]any); return a }

// oneLine 把多行/含制表符的文本压成单行，避免撑破 progress 列表的表格对齐。
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// reportStatus 给列表一行可读的“进展到哪了”：优先“在做什么”，其次“刚做完什么”，
// raw 兜底报告给原文摘要——而不是让计数恒显 0/0/0 把实际内容藏起来。
func reportStatus(r map[string]any) string {
	if isRawReport(r) {
		return "[raw] " + oneLine(rptStr(r["raw"]))
	}
	ip := strings.TrimSpace(rptStr(r["in_progress"]))
	if ip != "" && !strings.HasPrefix(ip, "无") && !strings.EqualFold(ip, "none") && !strings.EqualFold(ip, "n/a") {
		return "▶ " + oneLine(ip)
	}
	if d := rptArr(r["done"]); len(d) > 0 {
		return "✓ " + oneLine(rptStr(d[len(d)-1]))
	}
	if ip != "" {
		return oneLine(ip) // “无（…附注）”这类说明也比空好
	}
	if strings.TrimSpace(rptStr(r["next_prompt"])) != "" {
		return "…待接手"
	}
	return "—"
}

// renderReport 人读渲染单条进度报告（progress -show）：按“目标→进行中→完成→剩余→
// 阻塞→关键文件”排布，next_prompt 默认折叠，避免几千字接力 prompt 淹没实际进度。
func renderReport(e *ProgressEntry, full bool) {
	fmt.Printf("● %s  [%s]\n", e.Title, e.Key)
	if e.Dir != "" {
		fmt.Printf("  目录: %s\n", e.Dir)
	}
	if e.SessionID != "" {
		fmt.Printf("  会话: %s\n", e.SessionID)
	}
	if e.UpdatedAt != "" {
		fmt.Printf("  更新: %s\n", shortTime(e.UpdatedAt))
	}
	r := e.Report
	if isRawReport(r) {
		fmt.Println("\n[原文兜底 raw]")
		fmt.Println(rptStr(r["raw"]))
		return
	}
	if g := strings.TrimSpace(rptStr(r["goal"])); g != "" {
		fmt.Printf("\n目标: %s\n", g)
	}
	if ip := strings.TrimSpace(rptStr(r["in_progress"])); ip != "" {
		fmt.Printf("\n进行中: %s\n", ip)
	}
	printReportList("已完成", rptArr(r["done"]))
	printReportList("剩余", rptArr(r["remaining"]))
	printReportList("阻塞", rptArr(r["blockers"]))
	printReportList("关键文件", rptArr(r["key_files"]))
	if np := strings.TrimSpace(rptStr(r["next_prompt"])); np != "" {
		if full {
			fmt.Printf("\n接力 prompt:\n%s\n", np)
		} else {
			fmt.Printf("\n接力 prompt: (%d 字，加 -full 展开)\n", len([]rune(np)))
		}
	}
}

func printReportList(label string, items []any) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("\n%s (%d):\n", label, len(items))
	for _, it := range items {
		fmt.Printf("  • %s\n", rptStr(it))
	}
}

// progressIndex 载入进度报告并按 Key / SessionID 建索引，供 list 看板关联任务的最新进度。
func progressIndex(root string) (byKey, bySession map[string]*ProgressEntry) {
	byKey = map[string]*ProgressEntry{}
	bySession = map[string]*ProgressEntry{}
	for _, e := range loadProgressEntries(root) {
		if e.Key != "" {
			byKey[e.Key] = e
		}
		if e.SessionID != "" {
			bySession[e.SessionID] = e
		}
	}
	return byKey, bySession
}

// taskProgress 给 list 看板一行“最新进度概述”：优先该任务已回收的进度报告（按 ProgressKey /
// SessionID 关联）的现状，没有则回落到最近一步自动捕获的摘要。
func taskProgress(t *Task, byKey, bySession map[string]*ProgressEntry) string {
	if t.ProgressKey != "" {
		if e := byKey[t.ProgressKey]; e != nil {
			return reportStatus(e.Report)
		}
	}
	if t.SessionID != "" {
		if e := bySession[t.SessionID]; e != nil {
			return reportStatus(e.Report)
		}
	}
	if t.LastSummary != "" {
		return t.LastSummary
	}
	return "—"
}

func shortTime(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.Format("01-02 15:04")
	}
	return rfc
}

// cmdCmd 打印手动接管某任务的 claude 命令与当前步骤 prompt（想自己在终端里跑时用）。
func cmdCmd(args []string) error {
	fs := flag.NewFlagSet("cmd", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: claudego cmd <任务ID>")
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	t, err := findTask(root, fs.Arg(0))
	if err != nil {
		return err
	}
	if len(t.Prompts) == 0 {
		return fmt.Errorf("%s 没有 prompt", t.ID)
	}
	step := t.Step
	if step >= len(t.Prompts) {
		step = len(t.Prompts) - 1
	}
	parts := []string{"claude"}
	if t.Model != "" {
		parts = append(parts, "--model", t.Model)
	}
	if t.SessionID != "" {
		parts = append(parts, "--resume", t.SessionID)
	}
	fmt.Printf("# %s [%s] %s（第 %d/%d 步，状态 %s，优先级 %d）\n",
		t.ID, t.Type, t.Title, step+1, len(t.Prompts), zhStatus(t.Status), t.Priority)
	fmt.Printf("cd %s && %s\n", shellQuote(t.Dir), strings.Join(parts, " "))
	if t.MidStep && t.SessionID != "" {
		fmt.Printf("\n# 该会话在步骤中途被打断，进入后先发续跑提示：\n%s\n", cfg.ResumePrompt)
	} else {
		fmt.Printf("\n# 进入后粘贴当前步骤的 prompt：\n%s\n", injectLiveContext(root, t.ID, t.Prompts[step]))
	}
	fmt.Printf("\n# 手动接管前建议先挂起，避免调度器同时跑它: claudego hold %s\n", t.ID)
	return nil
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdQuota 展示 5 小时额度视图：队列账本、红线状态、外部用量源样本。
func cmdQuota(args []string) error {
	fs := flag.NewFlagSet("quota", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	now := time.Now()

	spent, byModel := queueWindowSpent(root, now)
	fmt.Printf("队列消耗（滑动 %d 小时窗口）：%.0f 加权 token\n", windowHours, spent)
	var models []string
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		fmt.Printf("  %-10s %.0f\n", m, byModel[m])
	}

	if cfg.QueueBudgetTokens > 0 {
		fmt.Printf("队列预算红线：%d（已用 %.0f%%）\n",
			cfg.QueueBudgetTokens, spent/float64(cfg.QueueBudgetTokens)*100)
		// 按窗口内燃烧速率估算触线时间（CodexBar 风格的耗尽预估，只算队列自己的消耗）
		var earliest int64
		for _, r := range loadUsage(root) {
			if r.At >= now.Add(-windowHours*time.Hour).Unix() && (earliest == 0 || r.At < earliest) {
				earliest = r.At
			}
		}
		if span := now.Unix() - earliest; earliest > 0 && span > 300 && spent > 0 && spent < float64(cfg.QueueBudgetTokens) {
			rate := spent / float64(span) // token/秒
			etaSec := (float64(cfg.QueueBudgetTokens) - spent) / rate
			fmt.Printf("  按当前速率约 %s 触线（%s）\n",
				time.Duration(etaSec*float64(time.Second)).Round(time.Minute),
				now.Add(time.Duration(etaSec*float64(time.Second))).Format("15:04"))
		}
	} else {
		fmt.Println("队列预算红线：未启用（config.json 的 queue_budget_tokens；先跑几天看上面的消耗量再定）")
	}

	_, effRP, _ := effectiveThresholds(cfg, now)
	if cfg.UsageFeed != "" {
		if s, err := latestFeedSample(cfg.UsageFeed); err != nil {
			fmt.Println("外部用量源：暂无 claude 样本（放行）——", err)
		} else if at, perr := time.Parse(time.RFC3339, s.SampledAt); perr == nil {
			age := now.Sub(at).Round(time.Minute)
			stale := ""
			if age > time.Duration(cfg.UsageFeedMaxAgeMin)*time.Minute {
				stale = "，已过期→放行"
			}
			rpNote := "红线未启用"
			if effRP > 0 {
				rpNote = fmt.Sprintf("当前红线 %d%%", effRP)
			}
			fmt.Printf("外部用量源：全局 5h 窗口已用 %d%%（%s，样本 %s 前%s）\n",
				s.UsedPercent, rpNote, age, stale)
		}
	} else {
		fmt.Println("外部用量源：未配置（usage_feed，支持 CodexBar usage-history.jsonl 格式）")
	}

	for _, w := range cfg.RedlineWindows {
		var parts []string
		if w.QueueBudgetTokens > 0 {
			parts = append(parts, fmt.Sprintf("队列预算 %d", w.QueueBudgetTokens))
		}
		if w.RedlinePercent > 0 {
			parts = append(parts, fmt.Sprintf("全局红线 %d%%", w.RedlinePercent))
		}
		mark := ""
		if inDailyWindow(now, w.From, w.To) {
			mark = "  ←当前时段生效"
		}
		fmt.Printf("时段红线 %s-%s：%s%s\n", w.From, w.To, strings.Join(parts, "，"), mark)
	}
	if cfg.RedlineLeadMin > 0 && len(cfg.RedlineWindows) > 0 {
		note := ""
		if hold, _ := preWindowHold(cfg, now); hold {
			note = "  ←缓冲期生效中"
		}
		fmt.Printf("前置缓冲：红线时段开始前 %d 分钟停发 claude 任务（codex 钉定不受影响）%s\n", cfg.RedlineLeadMin, note)
	}

	if cd := loadCooldown(root); cd.active(now) {
		fmt.Printf("⏳ 限额冷却中：%s 恢复（%s）\n", fmtClock(cd.UntilEpoch), fmtIn(cd.UntilEpoch, now))
	}
	if blocked, reason := budgetBlocked(root, cfg, now); blocked {
		fmt.Println("⛔ 红线生效中：" + reason)
	} else {
		fmt.Println("✓ 未触线，队列正常派发")
	}
	return nil
}

// ---- run / daemon ----

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	force := fs.Bool("force", false, "忽略限额冷却强行尝试")
	quiet := fs.Bool("quiet", false, "静默模式（launchd 用）")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	return tick(root, cfg, *force, *quiet)
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	return daemonLoop(root, cfg)
}

// ---- list ----

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	asJSON := fs.Bool("json", false, "输出 JSON")
	all := fs.Bool("all", false, "包含全部已结束任务")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	if _, err := loadConfig(root); err != nil {
		return err
	}
	tasks, err := loadTasks(root)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(tasks)
	}

	now := time.Now()
	if cd := loadCooldown(root); cd.active(now) {
		fmt.Printf("⏳ 限额冷却中：%s 恢复（还有 %s）\n\n", fmtClock(cd.UntilEpoch), fmtIn(cd.UntilEpoch, now))
	}
	if blocked, reason := budgetBlocked(root, mustConfig(root), now); blocked {
		fmt.Printf("⛔ 额度红线生效中：%s\n\n", reason)
	}
	if len(tasks) == 0 {
		fmt.Println("队列为空。用 claudego add / assemble / review 添加任务。")
		return nil
	}

	byStatus := map[string][]*Task{}
	for _, t := range tasks {
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}
	progByKey, progBySession := progressIndex(root)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t状态\t类型\t优先\t步骤\t标题 / 最新进度\t就绪/备注")
	printed := 0
	for _, st := range []string{statusRunning, statusLimitPaused, statusQueued, statusHeld, statusFailed, statusDone, statusCanceled} {
		group := byStatus[st]
		if (st == statusDone || st == statusCanceled) && !*all && len(group) > 5 {
			group = group[len(group)-5:]
		}
		for _, t := range group {
			note := "-"
			switch t.Status {
			case statusLimitPaused:
				note = fmt.Sprintf("%s 续跑（%s）", fmtClock(t.ResumeAtEpoch), fmtIn(t.ResumeAtEpoch, now))
			case statusQueued:
				if t.NotBeforeEpoch > now.Unix() {
					note = fmt.Sprintf("%s 重试", fmtClock(t.NotBeforeEpoch))
				} else {
					note = "就绪"
				}
			case statusFailed:
				note = truncate(t.LastError, 40)
			case statusDone:
				note = fmt.Sprintf("%d turns $%.2f", t.TurnsUsed, t.CostUSD)
			}
			if t.Runner != "" {
				note = "[" + t.Runner + "] " + note
			}
			step := fmt.Sprintf("%d/%d", t.Step, len(t.Prompts))
			if t.MidStep {
				step += "*"
			}
			desc := truncate(t.Title, 36)
			if prog := taskProgress(t, progByKey, progBySession); prog != "" && prog != "—" {
				desc = truncate(truncate(t.Title, 16)+" ▸ "+prog, 54)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", t.ID, zhStatus(t.Status), t.Type, t.Priority, step, desc, note)
			printed++
		}
	}
	w.Flush()
	if next := pickNext(mustConfig(root), tasks, now); next != nil {
		fmt.Printf("\n下一个将派发: %s（%s）\n", next.ID, next.Title)
	} else if wake := nextWake(tasks, now); !wake.IsZero() {
		fmt.Printf("\n暂无就绪任务，最早 %s 有任务就绪。\n", wake.Format("15:04"))
	}
	return nil
}

func mustConfig(root string) *Config {
	cfg, err := loadConfig(root)
	if err != nil {
		return defaultConfig("claude")
	}
	return cfg
}

func zhStatus(s string) string {
	m := map[string]string{
		statusQueued: "排队", statusRunning: "运行中", statusLimitPaused: "限额暂停",
		statusHeld: "已挂起", statusDone: "完成", statusFailed: "失败", statusCanceled: "已取消",
	}
	if v, ok := m[s]; ok {
		return v
	}
	return s
}

// ---- hold / release / retry / cancel ----

func cmdSetStatus(args []string, action string) error {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: claudego %s <任务ID>", action)
	}
	root := resolveRoot(*rootFlag)
	t, err := findTask(root, fs.Arg(0))
	if err != nil {
		return err
	}
	switch action {
	case "hold":
		if t.Status == statusRunning {
			return fmt.Errorf("%s 正在运行，无法挂起", t.ID)
		}
		t.Status = statusHeld
	case "release":
		if t.Status != statusHeld {
			return fmt.Errorf("%s 不在挂起状态（当前: %s）", t.ID, t.Status)
		}
		t.Status = statusQueued
		t.NotBeforeEpoch = 0
	case "retry":
		if !t.terminal() && t.Status != statusLimitPaused {
			return fmt.Errorf("%s 当前状态 %s 无需 retry", t.ID, t.Status)
		}
		t.Status = statusQueued
		t.Attempts = 0
		t.NotBeforeEpoch = 0
		t.ResumeAtEpoch = 0
		// 已跑完的任务 retry = 重跑：重置步数，否则会空转直接标完成。
		if t.Step >= len(t.Prompts) {
			t.Step = 0
			t.MidStep = false
		}
	case "cancel":
		t.Status = statusCanceled
		t.touch()
		if err := saveTask(root, t); err != nil {
			return err
		}
		if err := archiveTask(root, t); err != nil {
			return err
		}
		fmt.Printf("%s 已取消并归档。\n", t.ID)
		return nil
	}
	t.touch()
	if err := saveTask(root, t); err != nil {
		return err
	}
	fmt.Printf("%s -> %s\n", t.ID, zhStatus(t.Status))
	return nil
}

// ---- log / clean ----

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	n := fs.Int("n", 60, "显示最后 N 行")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: claudego log <任务ID> [-n 60]")
	}
	root := resolveRoot(*rootFlag)
	t, err := findTask(root, fs.Arg(0))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(taskLogPath(root, t.ID))
	if err != nil {
		return fmt.Errorf("该任务还没有日志（%s）", taskLogPath(root, t.ID))
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

func cmdClean(args []string) error {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	tasks, err := loadTasks(root)
	if err != nil {
		return err
	}
	moved := 0
	for _, t := range tasks {
		if t.terminal() {
			if err := archiveTask(root, t); err != nil {
				return err
			}
			moved++
		}
	}
	fmt.Printf("已归档 %d 个任务到 %s\n", moved, archiveDir(root))
	return nil
}

// ---- launchd / doctor ----

func cmdInstallLaunchd(args []string) error {
	fs := flag.NewFlagSet("install-launchd", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	interval := fs.Int("interval", 0, "轮询间隔秒数（默认取配置 poll_interval_sec）")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	sec := *interval
	if sec <= 0 {
		sec = cfg.PollIntervalSec
	}
	return installLaunchd(root, sec)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	ok := true
	check := func(name string, err error, hint string) {
		if err == nil {
			fmt.Printf("  ✔ %s\n", name)
		} else {
			ok = false
			fmt.Printf("  ✖ %s: %v\n      %s\n", name, err, hint)
		}
	}
	fmt.Println("claudego doctor")
	fmt.Println("数据目录:", root)

	cfg, err := loadConfig(root)
	check("配置文件", err, "运行 claudego init")
	if cfg != nil {
		_, err = os.Stat(cfg.ClaudeBin)
		if err != nil {
			_, err = exec.LookPath(cfg.ClaudeBin)
		}
		check("claude 可执行文件 ("+cfg.ClaudeBin+")", err, "确认 claude CLI 已安装，或修改 config.json 的 claude_bin")
	}
	_, err = os.Stat(tasksDir(root))
	check("任务目录", err, "运行 claudego init")

	if pp, err := plistPath(); err == nil {
		if _, err := os.Stat(pp); err == nil {
			fmt.Printf("  ✔ launchd 定时器已安装 (%s)\n", pp)
		} else {
			fmt.Println("  - launchd 定时器未安装（可运行 claudego install-launchd）")
		}
	}
	if fi, err := os.Stat(lockPath(root)); err == nil {
		fmt.Printf("  - 存在运行锁（%s，%s 前）\n", lockPath(root), time.Since(fi.ModTime()).Round(time.Second))
	}
	now := time.Now()
	if cd := loadCooldown(root); cd.active(now) {
		fmt.Printf("  - 限额冷却中，%s 恢复\n", fmtClock(cd.UntilEpoch))
	}
	if tasks, err := loadTasks(root); err == nil {
		counts := map[string]int{}
		for _, t := range tasks {
			counts[t.Status]++
		}
		fmt.Printf("  - 任务: %d 排队, %d 限额暂停, %d 运行中, %d 完成, %d 失败\n",
			counts[statusQueued], counts[statusLimitPaused], counts[statusRunning], counts[statusDone], counts[statusFailed])
	}
	if !ok {
		return fmt.Errorf("存在需要处理的问题")
	}
	return nil
}

// ---- helpers ----

var stepSplitRe = regexp.MustCompile(`(?m)^\s*---\s*$`)

func splitSteps(s string) []string {
	var out []string
	for _, part := range stepSplitRe.Split(s, -1) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func resolveDir(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[2:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("工作目录不存在: %s", abs)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("不是目录: %s", abs)
	}
	return abs, nil
}

func orDefaultTitle(title, prompt string) string {
	if title != "" {
		return title
	}
	return truncate(strings.Join(strings.Fields(prompt), " "), 48)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
