package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// 在跑执行器的进程登记簿。执行器自成进程组（见 setupProcGroup）后不再随
// claudego 的前台进程组收到终端信号——Ctrl-C/SIGTERM 若不接管，claudego 一死
// 它们就孤儿化继续烧额度、占工作目录（与 cancel 不杀进程是同一类病）。
var (
	procMu          sync.Mutex
	procGroups      = map[int]bool{}
	killHandlerOnce sync.Once
)

// runCmdRegistered 代替 cmd.Run：把子进程登记在册，供信号处理器连坐击杀。
func runCmdRegistered(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	procMu.Lock()
	procGroups[pid] = true
	procMu.Unlock()
	defer func() {
		procMu.Lock()
		delete(procGroups, pid)
		procMu.Unlock()
	}()
	return cmd.Wait()
}

// installKillHandler 让 Ctrl-C/SIGTERM 先击杀全部在册执行器进程组再退出。
func installKillHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		procMu.Lock()
		for pid := range procGroups {
			_ = killProcGroup(pid)
		}
		procMu.Unlock()
		os.Exit(130)
	}()
}
