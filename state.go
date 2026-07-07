package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Cooldown 是全局限额冷却：5 小时窗口用尽后，所有 claude 调用都会失败，
// 所以冷却是全局的，而不是单个任务的属性。
type Cooldown struct {
	UntilEpoch int64  `json:"until_epoch"`
	Reason     string `json:"reason"`
	SetAt      string `json:"set_at"`
}

func loadCooldown(root string) *Cooldown {
	data, err := os.ReadFile(cooldownPath(root))
	if err != nil {
		return nil
	}
	var c Cooldown
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func (c *Cooldown) active(now time.Time) bool {
	return c != nil && now.Unix() < c.UntilEpoch
}

func setCooldown(root string, until int64, reason string) {
	c := Cooldown{UntilEpoch: until, Reason: reason, SetAt: time.Now().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(c, "", "  ")
	_ = atomicWrite(cooldownPath(root), append(data, '\n'))
}

func clearCooldown(root string) { _ = os.Remove(cooldownPath(root)) }

type lockInfo struct {
	PID int    `json:"pid"`
	At  string `json:"at"`
}

// acquireLock 用 O_EXCL 抢占单实例锁；持锁进程已死或锁超龄则视为陈旧锁清除。
func acquireLock(root string, ttl time.Duration) bool {
	path := lockPath(root)
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			data, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
			_, _ = f.Write(data)
			_ = f.Close()
			return true
		}
		if !staleLock(path, ttl) {
			return false
		}
		_ = os.Remove(path)
	}
	return false
}

func staleLock(path string, ttl time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	if time.Since(fi.ModTime()) > ttl {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var li lockInfo
	if json.Unmarshal(data, &li) != nil || li.PID <= 0 {
		return true
	}
	proc, err := os.FindProcess(li.PID)
	if err != nil {
		return true
	}
	return proc.Signal(syscall.Signal(0)) != nil
}

func releaseLock(root string) { _ = os.Remove(lockPath(root)) }

func lockTTL(cfg *Config) time.Duration {
	ttl := time.Duration(cfg.StepTimeoutMin*3) * time.Minute
	if min := 3 * time.Hour; ttl < min {
		ttl = min
	}
	return ttl
}

func fmtClock(epoch int64) string {
	t := time.Unix(epoch, 0)
	if time.Until(t) > 20*time.Hour || time.Since(t) > 20*time.Hour {
		return t.Format("01-02 15:04")
	}
	return t.Format("15:04")
}

func fmtIn(epoch int64, now time.Time) string {
	d := time.Unix(epoch, 0).Sub(now).Round(time.Minute)
	if d < 0 {
		return "已到期"
	}
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
