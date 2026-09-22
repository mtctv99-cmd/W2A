//go:build windows

package browser

import (
	"fmt"
	"os/exec"
)

func SetProcessGroup(cmd *exec.Cmd) {
	// No-op or default process creation on Windows
}

func KillProcessGroup(pid int) {
	if pid > 0 {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid)).Run()
	}
}

func KillPID(pid int) {
	if pid > 0 {
		_ = exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
	}
}

func ReapZombies() {
	// No-op on Windows
}
