package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// rescueWaitDelay 把"进程成功退出但派生后台子进程握住 stdout 管道触发 WaitDelay"的
// exec.ErrWaitDelay 救回为 nil。cmd.Wait 因孙进程（ssh ControlPersist 后台 mux、rsync -e ssh、
// MCP/探针脚本等）吊住管道写端，等到 setupProcGroup 设的 WaitDelay(10s)后返回 ErrWaitDelay，
// 但进程本身已 exit 0（ProcessState.Success()）。不救则：runReviewSync 把成功的同步误判失败→
// divert 每轮被收掉、分流特性静默失效；本地执行器（invokeClaude/invokeCodex）把在手的有效结果
// 当失败重试。真正的超时/非零退出 ProcessState.Success()=false，不受本救援影响。
// 远端执行器（invokeRemoteClaude/invokeRemoteCodex）自有"有结果即成功"救援，不走此路。
func rescueWaitDelay(err error, cmd *exec.Cmd) error {
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}

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
