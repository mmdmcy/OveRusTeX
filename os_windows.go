package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

var (
	shell32                  = syscall.NewLazyDLL("shell32.dll")
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	procSHGetKnownFolderPath = shell32.NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	folderIDDocuments        = syscall.GUID{Data1: 0xFDD39AD0, Data2: 0x238F, Data3: 0x46AF, Data4: [8]byte{0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7}}
)

// knownDocumentsFolder asks Windows for FOLDERID_Documents, as dirs does
// (it follows a Documents folder moved to OneDrive or another drive).
func knownDocumentsFolder() (string, bool) {
	var p *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&folderIDDocuments)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if p != nil {
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	}
	if r != 0 || p == nil {
		return "", false
	}
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(p), 2*n)) != 0 {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n)), true
}

// hideWindow keeps a TeX run from flashing a console window, like the Rust
// version's CREATE_NO_WINDOW.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

func browserCommand(url string) *exec.Cmd {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	return cmd
}

// exitStatusText matches Rust's ExitStatus display on Windows.
func exitStatusText(state *os.ProcessState) string {
	return fmt.Sprintf("exit code: %d", state.ExitCode())
}

// pickerRoots lists the home folder, Documents and every drive letter.
func pickerRoots() []pickerRoot {
	roots := homeRoots()
	for c := 'C'; c <= 'Z'; c++ {
		drive := string(c) + `:\`
		if info, err := os.Stat(drive); err == nil && info.IsDir() {
			roots = append(roots, pickerRoot{Label: string(c) + ":", Path: drive})
		}
	}
	return roots
}
