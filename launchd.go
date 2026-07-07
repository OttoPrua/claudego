package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const launchdLabel = "com.claudego.tick"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>--quiet</string>
        <string>--root</string>
        <string>%s</string>
    </array>
    <key>StartInterval</key><integer>%d</integer>
    <key>RunAtLoad</key><true/>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func installLaunchd(root string, intervalSec int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	pp, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		return err
	}
	logOut := filepath.Join(logsDir(root), "launchd.log")
	content := fmt.Sprintf(plistTemplate, launchdLabel, exe, root, intervalSec, logOut, logOut)
	if err := os.WriteFile(pp, []byte(content), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if out, err := exec.Command("launchctl", "load", "-w", pp).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load 失败: %v\n%s", err, out)
	}
	fmt.Printf("已安装 launchd 定时器（每 %d 秒）: %s\n二进制: %s\n数据目录: %s\n", intervalSec, pp, exe, root)
	fmt.Println("提示: 如果之后移动或重新编译了二进制到其他路径，需要重新运行 install-launchd。")
	return nil
}

func uninstallLaunchd() error {
	pp, err := plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("已卸载 launchd 定时器:", pp)
	return nil
}
