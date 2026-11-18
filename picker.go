package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The in-app picker stands in for the Rust version's native file dialogs.
// It browses folders through /api/list.

type pickerRoot struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type pickerEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

type pickerListing struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent"` // "" at the top
	Sep     string        `json:"sep"`
	Entries []pickerEntry `json:"entries"`
	Roots   []pickerRoot  `json:"roots"`
}

func homeRoots() []pickerRoot {
	var roots []pickerRoot
	if home := homeDir(); home != "" {
		roots = append(roots, pickerRoot{Label: "Home", Path: home})
	}
	if docs, ok := documentDir(); ok {
		if info, err := os.Stat(docs); err == nil && info.IsDir() {
			roots = append(roots, pickerRoot{Label: "Documents", Path: docs})
		}
	}
	return roots
}

// listFolder lists one folder for the picker: folders first, then files,
// by name, hidden (dot) names left out. An empty path means the home
// folder; a path that does not exist starts at its nearest existing parent,
// as a native dialog does.
func listFolder(path string) (pickerListing, error) {
	if path == "" {
		path = homeDir()
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return pickerListing{}, fmt.Errorf("not an absolute path: %q", path)
	}
	path = filepath.Clean(path)
	for {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return pickerListing{}, fmt.Errorf("no folder found at %s", path)
		}
		path = parent
	}
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return pickerListing{}, wrapf(err, "failed to read %s", path)
	}
	entries := []pickerEntry{}
	for _, e := range dirEntries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(path, e.Name())
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			isDir = info.IsDir()
		}
		entries = append(entries, pickerEntry{Name: e.Name(), Path: full, IsDir: isDir})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	return pickerListing{Path: path, Parent: parent, Sep: string(filepath.Separator), Entries: entries, Roots: pickerRoots()}, nil
}
