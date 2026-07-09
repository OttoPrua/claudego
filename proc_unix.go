//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setupProcGroup 让执行器子进程自成进程组，取消/超时时可整组击杀：
// claude 会派生 bash 等孙进程，只杀直接子进程会留孤儿继续改仓库、烧额度
// （ssh 同路径——杀本地 ssh 释放槽位与目录锁；远端进程杀不到，只能断连）。
// WaitDelay 兜底：组内进程若 setsid 逃逸并吊住 stdout 管道，击杀后 10s 强制收尾。
func setupProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killProcGroup(cmd.Process.Pid)
	}
}

func killProcGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}
