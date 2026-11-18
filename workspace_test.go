package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The values below come from the Rust version's own DefaultHasher and
// document_cache_dir, run on the same inputs.
func TestRustStrHashMatchesRust(t *testing.T) {
	cases := map[string]uint64{
		"":                           0x30406ea523c53def,
		"a":                          0x719b50b9a4f0e9f3,
		"file:/tmp/project/main.tex": 0x989b0613aaace8a9,
		"untitled:/home/sleep/Documents/OveRusTeX":                           0x54629f499811d665,
		"file:/tmp/ünïcode dir/a long path name to cross several blocks.tex": 0x5b66073c760ec5b7,
	}
	for in, want := range cases {
		if got := rustStrHash(in); got != want {
			t.Errorf("rustStrHash(%q) = %016x, want %016x", in, got, want)
		}
	}
}

func TestDocumentCacheDirMatchesRust(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Rust values were taken on Linux")
	}
	home := isolate(t)
	parent := filepath.Join(home, "cache", "OveRusTeX", "documents")
	cases := []struct{ root, file, want string }{
		{"/tmp/project", "/tmp/project/main.tex", "main-989b0613aaace8a9"},
		{"/tmp/project", "", "project-f75db4d890a1ecce"},
		{"/", "", "untitled-ae8c32e21cdfaab0"},
	}
	for _, c := range cases {
		if got := documentCacheDir(c.root, c.file); got != filepath.Join(parent, c.want) {
			t.Errorf("documentCacheDir(%q, %q) = %s, want %s", c.root, c.file, got, c.want)
		}
	}
}

// Ported from the Rust tests.
func TestDocumentCacheDirIsStablePerDocument(t *testing.T) {
	isolate(t)
	root := filepath.FromSlash("/tmp/project")
	first := documentCacheDir(root, filepath.Join(root, "main.tex"))
	second := documentCacheDir(root, filepath.Join(root, "notes.tex"))
	if first == second {
		t.Fatal("two documents share a cache folder")
	}
	if first != documentCacheDir(root, filepath.Join(root, "main.tex")) {
		t.Fatal("the cache folder changed for the same document")
	}
}

func TestNestedFileKeepsWorkspaceRoot(t *testing.T) {
	root := filepath.FromSlash("/tmp/project")
	if got := workspaceRootForPath(root, filepath.Join(root, "chapters", "intro", "main.tex")); got != root {
		t.Fatalf("got %s", got)
	}
}

func TestExternalFileSwitchesWorkspaceRoot(t *testing.T) {
	root := filepath.FromSlash("/tmp/project")
	if got := workspaceRootForPath(root, filepath.FromSlash("/tmp/other/main.tex")); got != filepath.FromSlash("/tmp/other") {
		t.Fatalf("got %s", got)
	}
	// A sibling whose name starts with the root's is outside it.
	if got := workspaceRootForPath(root, filepath.FromSlash("/tmp/project2/main.tex")); got != filepath.FromSlash("/tmp/project2") {
		t.Fatalf("got %s", got)
	}
}

func TestMissingWorkspaceIsEmptyTree(t *testing.T) {
	entries, err := workspaceEntries(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("got %v, %v", entries, err)
	}
}

func TestWorkspaceEntries(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"b.tex", "A.PNG", "notes.txt", "out.pdf", "Chapters/intro.tex", "chapters2/x.svg",
		".git/config", "target/x", ".overustex/old.tex", "a/b/c/d/e/f/g/h/deep.tex"} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(f)), "x")
	}
	entries, err := workspaceEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name+":"+e.Kind)
	}
	want := []string{"a:dir", "Chapters:dir", "chapters2:dir", "A.PNG:image", "b.tex:tex", "notes.txt:file", "out.pdf:pdf"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
	if got := entries[1].Children[0].RelativePath; got != "Chapters/intro.tex" {
		t.Fatalf("relative path %q", got)
	}
	// Depth: the Rust version lists levels 0 to 6 below the root.
	level := entries[0]
	depth := 0
	for len(level.Children) > 0 {
		level = level.Children[0]
		depth++
	}
	if level.Name != "g" || !level.IsDir || depth != 6 {
		t.Fatalf("deepest entry %s at depth %d", level.Name, depth)
	}
}

func TestStemAndExtension(t *testing.T) {
	cases := []struct{ in, stem, ext, pdf string }{
		{"main.tex", "main", "tex", "main.pdf"},
		{"archive.tar.gz", "archive.tar", "gz", "archive.tar.pdf"},
		{"notes", "notes", "", "notes.pdf"},
		{".hidden", ".hidden", "", ".hidden.pdf"},
		{"trail.", "trail", "", "trail.pdf"},
	}
	for _, c := range cases {
		stem, ext, _ := splitStem(c.in)
		if stem != c.stem || ext != c.ext {
			t.Errorf("splitStem(%q) = %q, %q", c.in, stem, ext)
		}
		if got := withExtension(filepath.Join("dir", c.in), "pdf"); got != filepath.Join("dir", c.pdf) {
			t.Errorf("withExtension(%q) = %q", c.in, got)
		}
	}
}

func TestSanitizeJobName(t *testing.T) {
	cases := map[string]string{"main": "main", "my thesis": "my_thesis", "ü-é_1": "_-__1", "": "main", "a.b": "a_b"}
	for in, want := range cases {
		if got := sanitizeJobName(in); got != want {
			t.Errorf("sanitizeJobName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWorkspaceFilePathRules(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(root, "chapters", "a.tex"), "a")
	writeFile(t, filepath.Join(outside, "secret.tex"), "s")

	got, err := workspaceFilePath(root, "chapters/a.tex")
	if err != nil || got != filepath.Join(root, "chapters", "a.tex") {
		t.Fatalf("inside: %s, %v", got, err)
	}
	if _, err := workspaceFilePath(root, "chapters/../chapters/a.tex"); err != nil {
		t.Fatalf("a .. that stays inside should pass: %v", err)
	}
	for _, bad := range []string{"../" + filepath.Base(outside) + "/secret.tex", filepath.Join(outside, "secret.tex"), "", "chapters/../../x.tex"} {
		if _, err := workspaceFilePath(root, bad); !errors.Is(err, errOutsideWorkspace) {
			t.Errorf("%q: want errOutsideWorkspace, got %v", bad, err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.tex"), filepath.Join(root, "link.tex")); err != nil {
			t.Fatal(err)
		}
		if _, err := workspaceFilePath(root, "link.tex"); !errors.Is(err, errOutsideWorkspace) {
			t.Errorf("symlink out of the workspace: got %v", err)
		}
		if err := os.Symlink(filepath.Join(root, "chapters", "a.tex"), filepath.Join(root, "inner.tex")); err != nil {
			t.Fatal(err)
		}
		if _, err := workspaceFilePath(root, "inner.tex"); err != nil {
			t.Errorf("symlink inside the workspace: %v", err)
		}
	}
}
