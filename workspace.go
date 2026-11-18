package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const legacyWorkspaceDirName = ".overustex"

// WorkspaceEntry is one row of the explorer tree.
type WorkspaceEntry struct {
	Name         string           `json:"name"`
	RelativePath string           `json:"relative_path"`
	IsDir        bool             `json:"is_dir"`
	Kind         string           `json:"kind"`
	Children     []WorkspaceEntry `json:"children"`
}

// workspaceEntries lists root six levels deep, folders first, skipping
// .overustex, .git and target. A missing root is an empty tree.
func workspaceEntries(root string) ([]WorkspaceEntry, error) {
	if _, err := os.Stat(root); err != nil {
		return []WorkspaceEntry{}, nil
	}
	return workspaceEntriesRecursive(root, root, 0)
}

func workspaceEntriesRecursive(root, current string, depth int) ([]WorkspaceEntry, error) {
	items := []WorkspaceEntry{}
	if depth > 6 {
		return items, nil
	}
	dirEntries, err := os.ReadDir(current)
	if err != nil {
		return nil, wrapf(err, "failed to read %s", current)
	}
	kept := dirEntries[:0]
	for _, e := range dirEntries {
		name := e.Name()
		if name != legacyWorkspaceDirName && name != ".git" && name != "target" {
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.IsDir() != b.IsDir() {
			return a.IsDir()
		}
		return asciiLower(a.Name()) < asciiLower(b.Name())
	})
	for _, e := range kept {
		path := filepath.Join(current, e.Name())
		isDir := e.IsDir()
		children := []WorkspaceEntry{}
		if isDir {
			if children, err = workspaceEntriesRecursive(root, path, depth+1); err != nil {
				return nil, err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		items = append(items, WorkspaceEntry{
			Name:         e.Name(),
			RelativePath: strings.ReplaceAll(filepath.ToSlash(rel), `\`, "/"),
			IsDir:        isDir,
			Kind:         workspaceKind(path, isDir),
			Children:     children,
		})
	}
	return items, nil
}

func workspaceKind(path string, isDir bool) string {
	if isDir {
		return "dir"
	}
	_, ext, ok := splitStem(filepath.Base(path))
	if !ok {
		return "file"
	}
	switch asciiLower(ext) {
	case "tex":
		return "tex"
	case "png", "jpg", "jpeg", "gif", "bmp", "svg":
		return "image"
	case "pdf":
		return "pdf"
	}
	return "file"
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// splitStem splits a file name the way Rust's Path::file_stem and
// Path::extension do: on the last dot, except that a name starting with its
// only dot (".bashrc") has no extension.
func splitStem(name string) (stem, ext string, hasExt bool) {
	if name == ".." {
		return name, "", false
	}
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return name, "", false
	}
	return name[:i], name[i+1:], true
}

// withExtension is Rust's Path::with_extension.
func withExtension(path, ext string) string {
	dir, name := filepath.Split(path)
	stem, _, _ := splitStem(name)
	return dir + stem + "." + ext
}

// pathWithin reports whether p is root or inside it, comparing whole path
// components like Rust's Path::starts_with.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || filepath.IsLocal(rel)
}

// workspaceRootForPath keeps the workspace root when path is inside it and
// otherwise switches to the file's folder.
func workspaceRootForPath(currentRoot, path string) string {
	if pathWithin(currentRoot, path) {
		return currentRoot
	}
	parent := filepath.Dir(path)
	if parent == path {
		return defaultWorkspaceRoot()
	}
	return parent
}

var errOutsideWorkspace = errors.New("the path is not inside the workspace")

// workspaceFilePath resolves a path the explorer sent. It must be relative,
// stay inside the workspace after cleaning, and, with symlinks resolved,
// still point inside the workspace.
func workspaceFilePath(root, rel string) (string, error) {
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("%w: %s", errOutsideWorkspace, rel)
	}
	full := filepath.Join(root, local)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", wrapf(err, "failed to read %s", root)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", wrapf(err, "failed to read %s", full)
	}
	if !pathWithin(realRoot, realFull) {
		return "", fmt.Errorf("%w: %s", errOutsideWorkspace, rel)
	}
	return full, nil
}

// sanitizeJobName keeps ASCII letters, digits, '-' and '_' and turns every
// other character into '_'.
func sanitizeJobName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "main"
	}
	return b.String()
}

// documentCacheDir is documents/<label>-<hash> in the app cache. The hash is
// the one Rust's DefaultHasher gives the same identity string, so a
// document keeps its cache folder from the Rust version.
func documentCacheDir(workspaceRoot, currentFile string) string {
	identity := "untitled:" + workspaceRoot
	label := ""
	if currentFile != "" {
		identity = "file:" + currentFile
		label, _, _ = splitStem(filepath.Base(currentFile))
	} else if base := filepath.Base(workspaceRoot); base != string(filepath.Separator) && base != "." && filepath.Dir(workspaceRoot) != workspaceRoot {
		label = base
	}
	if label == "" {
		label = "untitled"
	}
	return filepath.Join(documentCacheParent(), fmt.Sprintf("%s-%016x", sanitizeJobName(label), rustStrHash(identity)))
}

// rustStrHash is Rust's DefaultHasher (SipHash-1-3, zero keys) over a str:
// its bytes followed by 0xff.
func rustStrHash(s string) uint64 {
	msg := append([]byte(s), 0xff)
	v0 := uint64(0x736f6d6570736575)
	v1 := uint64(0x646f72616e646f6d)
	v2 := uint64(0x6c7967656e657261)
	v3 := uint64(0x7465646279746573)
	round := func() {
		v0 += v1
		v1 = bits.RotateLeft64(v1, 13)
		v1 ^= v0
		v0 = bits.RotateLeft64(v0, 32)
		v2 += v3
		v3 = bits.RotateLeft64(v3, 16)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft64(v3, 21)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft64(v1, 17)
		v1 ^= v2
		v2 = bits.RotateLeft64(v2, 32)
	}
	n := len(msg)
	for len(msg) >= 8 {
		m := binary.LittleEndian.Uint64(msg)
		v3 ^= m
		round()
		v0 ^= m
		msg = msg[8:]
	}
	b := uint64(n&0xff) << 56
	for i, c := range msg {
		b |= uint64(c) << (8 * i)
	}
	v3 ^= b
	round()
	v0 ^= b
	v2 ^= 0xff
	round()
	round()
	round()
	return v0 ^ v1 ^ v2 ^ v3
}

// ctxError carries a context line over a cause, like anyhow's Context: the
// outer line is the short detail, Error() the whole chain.
type ctxError struct {
	msg string
	err error
}

func (e *ctxError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *ctxError) Unwrap() error { return e.err }

func wrapf(err error, format string, args ...any) error {
	return &ctxError{msg: fmt.Sprintf(format, args...), err: err}
}

// outerMessage is the outermost context line of err.
func outerMessage(err error) string {
	if c, ok := err.(*ctxError); ok {
		return c.msg
	}
	return err.Error()
}
