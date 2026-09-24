package main

import (
	"os"
	"os/exec"
	"syscall"
)

func openURL(url string) error {
	return start(exec.Command("rundll32", "url.dll,FileProtocolHandler", url))
}

// reveal selects a file in an Explorer window, or opens a folder.
func reveal(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return start(exec.Command("explorer", path))
	}
	// Explorer parses its own command line and wants the path quoted
	// after the comma, which Go's argument quoting would not produce.
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer /select,"` + path + `"`}
	return start(cmd)
}

func computerName() string {
	return hostName()
}
