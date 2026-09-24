//go:build !darwin && !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

func openURL(url string) error {
	return start(exec.Command("xdg-open", url))
}

// reveal opens the folder that holds a file, or the folder itself. There
// is no portable way to ask Linux file managers to select a file.
func reveal(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}
	return start(exec.Command("xdg-open", path))
}

func computerName() string {
	return hostName()
}
