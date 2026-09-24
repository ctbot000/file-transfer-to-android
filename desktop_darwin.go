package main

import (
	"os"
	"os/exec"
	"strings"
)

func openURL(url string) error {
	return start(exec.Command("open", url))
}

// reveal selects a file in a Finder window, or opens a folder.
func reveal(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return start(exec.Command("open", path))
	}
	return start(exec.Command("open", "-R", path))
}

// computerName returns the name set in System Settings, such as "Jo's
// MacBook Air", which reads better on the phone than the host name.
func computerName() string {
	if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	return hostName()
}
