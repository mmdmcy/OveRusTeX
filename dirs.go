package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
)

// The folder names and lookups below match the Rust version (the dirs crate),
// so the cache and the default workspace of an existing install carry over.

const appStorageDirName = "OveRusTeX"

// homeDir is the user's home folder, or "" when there is none.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// userCacheDir follows dirs::cache_dir: $XDG_CACHE_HOME when it is absolute,
// else ~/.cache on Linux, ~/Library/Caches on macOS, %LocalAppData% on Windows.
func userCacheDir() (string, bool) {
	switch runtime.GOOS {
	case "windows", "darwin":
		dir, err := os.UserCacheDir()
		return dir, err == nil && dir != ""
	}
	if dir := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		return dir, true
	}
	if home := homeDir(); home != "" {
		return filepath.Join(home, ".cache"), true
	}
	return "", false
}

// userDataLocalDir follows dirs::data_local_dir, the Rust version's fallback
// when there is no cache folder.
func userDataLocalDir() (string, bool) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return dir, true
		}
		return "", false
	case "darwin":
		if home := homeDir(); home != "" {
			return filepath.Join(home, "Library", "Application Support"), true
		}
		return "", false
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir, true
	}
	if home := homeDir(); home != "" {
		return filepath.Join(home, ".local", "share"), true
	}
	return "", false
}

// appCacheDir is <cache>/OveRusTeX, as in the Rust version.
func appCacheDir() string {
	base, ok := userCacheDir()
	if !ok {
		base, ok = userDataLocalDir()
	}
	if !ok {
		base = os.TempDir()
	}
	return filepath.Join(base, appStorageDirName)
}

func documentCacheParent() string {
	return filepath.Join(appCacheDir(), "documents")
}

// documentDir follows dirs::document_dir: XDG_DOCUMENTS_DIR from
// user-dirs.dirs on Linux, ~/Documents on macOS, the Documents known folder
// on Windows.
func documentDir() (string, bool) {
	switch runtime.GOOS {
	case "windows":
		return knownDocumentsFolder()
	case "darwin":
		if home := homeDir(); home != "" {
			return filepath.Join(home, "Documents"), true
		}
		return "", false
	}
	home := homeDir()
	if home == "" {
		return "", false
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	data, err := os.ReadFile(filepath.Join(config, "user-dirs.dirs"))
	if err != nil {
		return "", false
	}
	return parseUserDir(home, "DOCUMENTS", data)
}

// parseUserDir reads one XDG_<name>_DIR line the way dirs-sys does: double
// quotes only, "$HOME/..." or an absolute path, "$HOME/" meaning disabled,
// the first matching line wins.
func parseUserDir(home, name string, data []byte) (string, bool) {
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		key, value, ok := bytes.Cut(line, []byte("="))
		if !ok {
			continue
		}
		key = bytes.Trim(key, " \t")
		if string(key) != "XDG_"+name+"_DIR" {
			continue
		}
		value = bytes.Trim(value, " \t")
		if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			continue
		}
		value = value[1 : len(value)-1]
		relative := false
		switch {
		case string(value) == "$HOME/":
			continue
		case bytes.HasPrefix(value, []byte("$HOME/")):
			value = value[len("$HOME/"):]
			relative = true
		case bytes.HasPrefix(value, []byte("/")):
		default:
			continue
		}
		path := shellUnescape(value)
		if relative {
			return filepath.Join(home, path), true
		}
		return path, true
	}
	return "", false
}

func shellUnescape(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' {
			i++
			if i < len(b) {
				out = append(out, b[i])
			}
			continue
		}
		out = append(out, b[i])
	}
	return string(out)
}

// defaultWorkspaceRoot is <Documents>/OveRusTeX, or <current dir>/OveRusTeX
// when there is no Documents folder.
func defaultWorkspaceRoot() string {
	base, ok := documentDir()
	if !ok {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		base = cwd
	}
	return filepath.Join(base, appStorageDirName)
}

func defaultRunPath() string {
	return filepath.Join(defaultWorkspaceRoot(), "main.tex")
}
