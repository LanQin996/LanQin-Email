package maintenance

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func prepareComposeProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
}

func composeRootIdentity(name string) (string, error) {
	before, err := os.Lstat(name)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return "", errCompose
	}
	file, err := os.Open(name)
	if err != nil {
		return "", errCompose
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errCompose
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errCompose
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}
