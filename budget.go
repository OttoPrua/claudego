package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// windowHours 是 Claude 订阅限额窗口长度。账本用滑动窗口近似官方的固定窗口，
// 边界附近略保守，但不需要探测窗口起点。
const windowHours = 5

// usageRec 是一次 claude 调用的额度消耗记录（本地账本 usage.json）。
type usageRec struct {
	At       int64   `json:"at"`
	TaskID   string  `json:"task"`
	Model    string  `json:"model"`
	Weighted float64 `json:"wtok"`
}

func modelWeight(cfg *Config, model string) float64 {
	if w, ok := cfg.ModelWeights[model]; ok && w > 0 {
		return w
	}
	if w, ok := cfg.ModelWeights["default"]; ok && w > 0 {
		return w
	}
	return 1
}

// weightedTokens 把一次调用的 token 用量折算成加权额度。
// 近似规则：未命中缓存的输入与输出全价，缓存读取按 1 折计，再乘模型权重。
func weightedTokens(cfg *Config, model string, u *usageInfo) float64 {
	if u == nil {
		return 0
	}
	raw := float64(u.InputTokens+u.CacheCreationInputTokens+u.OutputTokens) + 0.1*float64(u.CacheReadInputTokens)
	return raw * modelWeight(cfg, model)
}

func loadUsage(root string) []usageRec {
	data, err := os.ReadFile(usagePath(root))
	if err != nil {
		return nil
	}
	var recs []usageRec
	if json.Unmarshal(data, &recs) != nil {
		return nil
	}
	return recs
}

// usageMu 保护账本的读-改-写：并行任务同时记账时不能互相覆盖。
var usageMu sync.Mutex

// appendUsage 记账并顺手把窗口外的旧记录剪掉（多留 1 小时算燃烧速率用）。
func appendUsage(root string, cfg *Config, t *Task, u *usageInfo) {
	usageMu.Lock()
	defer usageMu.Unlock()
	w := weightedTokens(cfg, t.Model, u)
	if w <= 0 {
		return
	}
	now := time.Now()
	keep := now.Add(-(windowHours + 1) * time.Hour).Unix()
	var recs []usageRec
	for _, r := range loadUsage(root) {
		if r.At >= keep {
			recs = append(recs, r)
		}
	}
	recs = append(recs, usageRec{At: now.Unix(), TaskID: t.ID, Model: t.Model, Weighted: w})
	if data, err := json.Marshal(recs); err == nil {
		_ = atomicWrite(usagePath(root), append(data, '\n'))
	}
}

// queueWindowSpent 返回滑动窗口内队列消耗的加权 token 与各模型分布。
func queueWindowSpent(root string, now time.Time) (float64, map[string]float64) {
	since := now.Add(-windowHours * time.Hour).Unix()
	total := 0.0
	byModel := map[string]float64{}
	for _, r := range loadUsage(root) {
		if r.At < since {
			continue
		}
		total += r.Weighted
		m := r.Model
		if m == "" {
			m = "(默认)"
		}
		byModel[m] += r.Weighted
	}
	return total, byModel
}

// ---- 外部全局用量源（CodexBar usage-history.jsonl 格式）----

type feedSample struct {
	Provider      string `json:"provider"`
	SampledAt     string `json:"sampledAt"`
	ResetsAt      string `json:"resetsAt"`
	UsedPercent   int    `json:"usedPercent"`
	WindowMinutes int    `json:"windowMinutes"`
	WindowKind    string `json:"windowKind"`
}

// latestFeedSample 取用量源里最新的 claude 5 小时窗口样本。
func latestFeedSample(path string) (*feedSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 只扫尾部 256KB，避免历史文件太大。
	if len(data) > 256*1024 {
		data = data[len(data)-256*1024:]
	}
	var best *feedSample
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, `"provider":"claude"`) {
			continue
		}
		var s feedSample
		if json.Unmarshal([]byte(line), &s) != nil || s.Provider != "claude" {
			continue
		}
		// 5 小时窗口：windowMinutes 300，或标记为 primary。
		if s.WindowMinutes != windowHours*60 && s.WindowKind != "primary" {
			continue
		}
		if best == nil || s.SampledAt > best.SampledAt {
			cp := s
			best = &cp
		}
	}
	if best == nil {
		return nil, fmt.Errorf("用量源里没有 claude 的 5 小时窗口样本")
	}
	return best, nil
}

// ---- 分时段红线 ----

func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inDailyWindow 判断 now 是否落在每日 [from, to) 时段内；from > to 表示跨零点。
func inDailyWindow(now time.Time, from, to string) bool {
	f, ok1 := parseHHMM(from)
	t, ok2 := parseHHMM(to)
	if !ok1 || !ok2 || f == t {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if f < t {
		return cur >= f && cur < t
	}
	return cur >= f || cur < t
}

// effectiveThresholds 返回此刻生效的红线阈值：时段内非零字段覆盖全局值。
// label 非空表示有时段命中（用于提示信息）。
func effectiveThresholds(cfg *Config, now time.Time) (qb int64, rp int, label string) {
	qb, rp = cfg.QueueBudgetTokens, cfg.RedlinePercent
	for _, w := range cfg.RedlineWindows {
		if inDailyWindow(now, w.From, w.To) {
			if w.QueueBudgetTokens > 0 {
				qb = w.QueueBudgetTokens
			}
			if w.RedlinePercent > 0 {
				rp = w.RedlinePercent
			}
			label = fmt.Sprintf("时段 %s-%s：", w.From, w.To)
		}
	}
	return qb, rp, label
}

// preWindowHold 判断当前是否处于某个红线时段的前置缓冲期：
// 时段开始前 RedlineLeadMin 分钟内不再起跑 claude 任务（跑起来的单步任务无法让位）。
func preWindowHold(cfg *Config, now time.Time) (bool, string) {
	if cfg.RedlineLeadMin <= 0 {
		return false, ""
	}
	cur := now.Hour()*60 + now.Minute()
	for _, w := range cfg.RedlineWindows {
		if w.RedlinePercent <= 0 && w.QueueBudgetTokens <= 0 {
			continue
		}
		f, ok := parseHHMM(w.From)
		if !ok {
			continue
		}
		start := (f - cfg.RedlineLeadMin + 1440) % 1440
		in := false
		if start < f {
			in = cur >= start && cur < f
		} else { // 缓冲跨零点
			in = cur >= start || cur < f
		}
		if in {
			return true, fmt.Sprintf("红线时段 %s-%s 前置缓冲（%d 分钟）：不再起跑 claude 任务，避免踩进预留窗口",
				w.From, w.To, cfg.RedlineLeadMin)
		}
	}
	return false, ""
}

// budgetBlocked 判定额度红线是否生效（true 则本轮不派发）。
// 两条通道相互独立：本地队列预算封顶；外部用量源按全局百分比停。外部样本过期放行。
func budgetBlocked(root string, cfg *Config, now time.Time) (bool, string) {
	if hold, reason := preWindowHold(cfg, now); hold {
		return true, reason
	}
	qb, rp, label := effectiveThresholds(cfg, now)
	if qb > 0 {
		spent, _ := queueWindowSpent(root, now)
		if spent >= float64(qb) {
			return true, fmt.Sprintf("%s队列已用 %.0f/%d 加权 token（滑动 %dh 窗口），保底额度留给交互使用",
				label, spent, qb, windowHours)
		}
	}
	if rp > 0 && cfg.UsageFeed != "" {
		if s, err := latestFeedSample(cfg.UsageFeed); err == nil {
			if at, err := time.Parse(time.RFC3339, s.SampledAt); err == nil {
				age := now.Sub(at)
				maxAge := time.Duration(cfg.UsageFeedMaxAgeMin) * time.Minute
				if age <= maxAge && s.UsedPercent >= rp {
					return true, fmt.Sprintf("%s全局 5h 窗口已用 %d%%（红线 %d%%，样本 %s 前）",
						label, s.UsedPercent, rp, age.Round(time.Minute))
				}
			}
		}
	}
	return false, ""
}
