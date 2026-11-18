package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseUserDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("user-dirs.dirs is a Linux file")
	}
	home := "/home/u"
	cases := []struct {
		file string
		want string
		ok   bool
	}{
		{`XDG_DOCUMENTS_DIR="$HOME/Documents"`, "/home/u/Documents", true},
		{"# comment\n  XDG_DOCUMENTS_DIR = \"/data/docs\"  \n", "/data/docs", true},
		{`XDG_DOCUMENTS_DIR="$HOME/My\ Docs"`, "/home/u/My Docs", true},
		{`XDG_DOCUMENTS_DIR="$HOME/"`, "", false},
		{`XDG_DOCUMENTS_DIR=$HOME/Documents`, "", false},
		{`XDG_DOCUMENTS_DIR="relative"`, "", false},
		{"XDG_DOCUMENTS_DIR=\"/first\"\nXDG_DOCUMENTS_DIR=\"/second\"", "/first", true},
		{`XDG_DOWNLOAD_DIR="$HOME/Downloads"`, "", false},
	}
	for _, c := range cases {
		got, ok := parseUserDir(home, "DOCUMENTS", []byte(c.file))
		if ok != c.ok || got != filepath.FromSlash(c.want) {
			t.Errorf("%q: got %q %v, want %q %v", c.file, got, ok, c.want, c.ok)
		}
	}
}

func TestLocationsFollowTheRustVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG rules are Linux's")
	}
	home := isolate(t)
	if got, want := appCacheDir(), filepath.Join(home, "cache", "OveRusTeX"); got != want {
		t.Errorf("cache %s, want %s", got, want)
	}
	t.Setenv("XDG_CACHE_HOME", "relative/cache")
	if got, want := appCacheDir(), filepath.Join(home, ".cache", "OveRusTeX"); got != want {
		t.Errorf("a relative XDG_CACHE_HOME is ignored: got %s, want %s", got, want)
	}

	// No user-dirs.dirs: the workspace falls back to the current folder.
	cwd, _ := filepath.Abs(".")
	if got, want := defaultWorkspaceRoot(), filepath.Join(cwd, "OveRusTeX"); got != want {
		t.Errorf("workspace %s, want %s", got, want)
	}
	writeFile(t, filepath.Join(home, "config", "user-dirs.dirs"), `XDG_DOCUMENTS_DIR="$HOME/Docs"`+"\n")
	if got, want := defaultRunPath(), filepath.Join(home, "Docs", "OveRusTeX", "main.tex"); got != want {
		t.Errorf("run path %s, want %s", got, want)
	}
}
