//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

func knownDocumentsFolder() (string, bool) { return "", false }

func hideWindow(*exec.Cmd) {}

func browserCommand(url string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}

// exitStatusText matches Rust's ExitStatus display on Unix.
func exitStatusText(state *os.ProcessState) string {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return fmt.Sprintf("signal: %d (%s)", int(ws.Signal()), ws.Signal())
	}
	return fmt.Sprintf("exit status: %d", state.ExitCode())
}

func pickerRoots() []pickerRoot {
	return append(homeRoots(), pickerRoot{Label: "Computer", Path: "/"})
}
