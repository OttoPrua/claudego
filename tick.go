package main

import (
	"fmt"
	"math/rand"
	"os"
	"time"
)

// tick 是调度的最小单元：抢锁 → 排空队列（drain）。
// 每轮循环在冷却/红线允许的前提下，把就绪任务派发到并行槽位（最多 max_parallel 个），
// 全部跑完或没有可派发任务时才返回。同一工作目录同一时刻只跑一个任务，
// 避免两个会话并发改同一个仓库。launchd 每隔 poll_interval_sec 调一次兜底。
func tick(root string, cfg *Config, force, quiet bool) error {
	if !acquireLock(root, lockTTL(cfg)) {
		if !quiet {
			fmt.Println("已有另一个 claudego 实例在运行，跳过本轮。")
		}
		return nil
	}
	defer releaseLock(root)

	maxPar := cfg.MaxParallel
	if maxPar < 1 {
		maxPar = 1
	}

	type doneMsg struct{ t *Task }
	ch := make(chan doneMsg)
	activeIDs := map[string]bool{}
	activeDirs := map[string]bool{}
	claimedDir := map[string]bool{} // 哪些在跑任务占用了目录互斥（只读类型不占用）
	// 只读类型（审核/进度回收）不写文件：既不占用同目录互斥，也不被互斥挡住——
	// 审核卡可与同仓下一批并行（依赖护栏在排批层：批内叶组互不依赖、不消费未过审契约）。
	readOnly := func(t *Task) bool { return t.Type == typeReview || t.Type == typeProgressPull }
	launched := 0

	report := func(t *Task) {
		if quiet {
			return
		}
		switch t.Status {
		case statusDone:
			fmt.Printf("✔ %s 完成（%d 步，%d turns，$%.4f）\n", t.ID, len(t.Prompts), t.TurnsUsed, t.CostUSD)
		case statusLimitPaused:
			fmt.Printf("⏸ %s 撞到用量限额，%s 自动续跑。\n", t.ID, fmtClock(t.ResumeAtEpoch))
		case statusFailed:
			fmt.Printf("✖ %s 失败: %s（claudego log %s 查看详情）\n", t.ID, t.LastError, t.ID)
		case statusQueued:
			fmt.Printf("↻ %s 让位/重试，%s 后再跑（第 %d 次）: %s\n", t.ID, fmtClock(t.NotBeforeEpoch), t.Attempts, t.LastError)
		}
	}

	for {
		now := time.Now()
		_ = os.Chtimes(lockPath(root), now, now) // 长时间 drain 时刷新锁，防止被当作陈旧锁清除

		blockReason := ""
		if cd := loadCooldown(root); !force && cd.active(now) {
			blockReason = fmt.Sprintf("限额冷却中：%s 恢复（还有 %s）", fmtClock(cd.UntilEpoch), fmtIn(cd.UntilEpoch, now))
		} else if blocked, reason := budgetBlocked(root, cfg, now); !force && blocked {
			blockReason = "额度红线：" + reason
		}

		// 有空槽时尽量填满并行槽位。每个候选独立决定执行器：
		//  - runner_pref=codex 的任务钉在 codex 上（不管 claude 忙闲，用独立 GPT 额度）；
		//  - claude 正常时其余任务走 claude；
		//  - claude 被冷却/红线拦住时，开启 fallback 的话把 codexEligible 的任务切给 codex。
		if len(activeIDs) < maxPar {
			tasks, err := loadTasks(root)
			if err != nil {
				if len(activeIDs) == 0 {
					return err
				}
			} else {
				viaCodex := map[string]bool{}
				var cands []*Task
				for _, t := range tasks {
					if activeIDs[t.ID] || (activeDirs[t.Dir] && !readOnly(t)) {
						continue
					}
					switch {
					case t.RemoteHost != "":
						// 远端 codex 执行器：SSH 到远端跑 codex，走自己的 GPT 额度，不受 claude 冷却/红线阻塞。
						// 需 codexEligible（单步/fresh、无会话）且已配置该主机；否则不派。useCodex 保持 false，
						// runTask 见 RemoteHost 自走 invokeRemoteCodex。
						if _, ok := cfg.RemoteHosts[t.RemoteHost]; !ok || !codexEligible(t) {
							continue
						}
					case t.PreferRunner == "codex" && cfg.CodexBin != "" && codexEligible(t):
						viaCodex[t.ID] = true
					case blockReason == "":
						// claude 可用，正常走
					case cfg.CodexFallback && cfg.CodexBin != "" && codexEligible(t) && !noFallback(cfg, t.Model):
						viaCodex[t.ID] = true
					default:
						continue // claude 被拦且没有 codex 出路
					}
					cands = append(cands, t)
				}
				if next := pickNext(cfg, cands, now); next != nil {
					useCodex := viaCodex[next.ID]
					activeIDs[next.ID] = true
					if !readOnly(next) {
						activeDirs[next.Dir] = true
						claimedDir[next.ID] = true
					}
					launched++
					if !quiet {
						runner := ""
						if useCodex {
							runner = "，runner=codex"
						}
						fmt.Printf("▶ 运行 %s [%s] %s（第 %d/%d 步，并行 %d/%d%s）\n",
							next.ID, next.Type, next.Title, next.Step+1, len(next.Prompts), len(activeIDs), maxPar, runner)
					}
					go func(t *Task, viaCodex bool) {
						if err := runTask(root, cfg, t, viaCodex); err != nil && !quiet {
							fmt.Printf("✖ %s 执行出错: %v\n", t.ID, err)
						}
						ch <- doneMsg{t}
					}(next, useCodex)
					continue // 尝试继续填下一个槽位
				}
			}
		}

		// 没有可再派发的：没有在跑的就收工，否则等一个跑完再看。
		if len(activeIDs) == 0 {
			if !quiet {
				switch {
				case blockReason != "":
					fmt.Println(blockReason + "（-force 可越线）")
				case launched == 0:
					tasks, _ := loadTasks(root)
					if wake := nextWake(tasks, time.Now()); !wake.IsZero() {
						fmt.Printf("暂无可运行任务，下一个将在 %s 就绪。\n", wake.Format("15:04"))
					} else {
						fmt.Println("队列为空。用 claudego add / assemble / review 添加任务。")
					}
				default:
					fmt.Printf("本轮共处理 %d 个任务。\n", launched)
				}
			}
			return nil
		}
		// 等一个任务完成；或定时超时后回到循环顶重扫队列——让 drain 期间新入队的任务
		// （尤其分离执行器，如远端主机的并行设计循环）能及时补进空闲槽位，
		// 而不必干等某个在跑的长任务结束（否则 Mac 与 5090 的并行设计线会被串行化）。
		select {
		case msg := <-ch:
			delete(activeIDs, msg.t.ID)
			if claimedDir[msg.t.ID] {
				delete(activeDirs, msg.t.Dir)
				delete(claimedDir, msg.t.ID)
			}
			report(msg.t)
		case <-time.After(15 * time.Second):
			// 重扫超时：不动任何在跑任务，回循环顶用空闲槽位尝试派发新就绪任务。
		}
	}
}

// noFallback 判断该模型的任务是否禁止降级到 codex（设计类模型宁可排队等 claude）。
func noFallback(cfg *Config, model string) bool {
	for _, m := range cfg.NoFallbackModels {
		if m == model {
			return true
		}
	}
	return false
}

func daemonLoop(root string, cfg *Config) error {
	fmt.Printf("claudego daemon 启动，每 %d 秒轮询一次，最多 %d 路并行（Ctrl-C 退出）。\n",
		cfg.PollIntervalSec, cfg.MaxParallel)
	for {
		if err := tick(root, cfg, false, false); err != nil {
			fmt.Println("tick 出错:", err)
		}
		jitter := time.Duration(rand.Intn(cfg.PollIntervalSec/10+1)) * time.Second
		time.Sleep(time.Duration(cfg.PollIntervalSec)*time.Second + jitter)
	}
}
