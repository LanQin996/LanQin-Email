//go:build !linux

package maintenance

import "os/exec"

// Production calls are Linux-only; non-Linux tests inject a private runner.
func prepareComposeProcess(command *exec.Cmd) {}

func composeRootIdentity(string) (string, error) { return "", errCompose }
