package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// dockerLogin logs docker in to the Team's OCI host, the Token on stdin.
// Skipped, with a reason, when docker is not installed.
func dockerLogin(host, token string) (bool, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return false, nil
	}
	cmd := exec.Command(path, "login", host, "--username", "token", "--password-stdin")
	cmd.Stdin = strings.NewReader(token)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return true, fmt.Errorf("docker login %s failed: %s", host, strings.TrimSpace(out.String()))
	}
	return true, nil
}

func dockerLogout(host string) {
	if path, err := exec.LookPath("docker"); err == nil {
		_ = exec.Command(path, "logout", host).Run()
	}
}
