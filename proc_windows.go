//go:build windows

package main

import (
	"os"
	"os/exec"
	"time"
)

// Windows 无 POSIX 进程组语义：取消/超时退回默认的单进程 Kill（孙进程可能残留），
// WaitDelay 防残留进程吊住 stdout 管道不放。
func setupProcGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 10 * time.Second
}

func killProcGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
