package main

import (
	"os"
	"os/exec"
	"strings"
)

// start runs a helper program in the background and reaps it when it exits.
func start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func hostName() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "this computer"
	}
	return strings.TrimSuffix(name, ".local")
}
