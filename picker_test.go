package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestListFolder(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.tex"), "")
	writeFile(t, filepath.Join(dir, "A.pdf"), "")
	writeFile(t, filepath.Join(dir, ".hidden.tex"), "")
	writeFile(t, filepath.Join(dir, "zeta", "x.tex"), "")
	writeFile(t, filepath.Join(dir, ".git", "config"), "")
	if runtime.GOOS != "windows" {
		os.Symlink(filepath.Join(dir, "zeta"), filepath.Join(dir, "link"))
	}

	listing, err := listFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range listing.Entries {
		kind := "file"
		if e.IsDir {
			kind = "dir"
		}
		got = append(got, e.Name+":"+kind)
		if e.Path != filepath.Join(dir, e.Name) {
			t.Errorf("path %s", e.Path)
		}
	}
	want := []string{"link:dir", "zeta:dir", "A.pdf:file", "b.tex:file"}
	if runtime.GOOS == "windows" {
		want = want[1:]
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if listing.Parent != filepath.Dir(dir) || listing.Sep != string(filepath.Separator) || len(listing.Roots) == 0 {
		t.Fatalf("listing %+v", listing)
	}

	// A folder that does not exist starts at its nearest parent.
	if listing, err = listFolder(filepath.Join(dir, "missing", "deeper")); err != nil || listing.Path != dir {
		t.Fatalf("missing: %s, %v", listing.Path, err)
	}
	// A file starts at its folder.
	if listing, err = listFolder(filepath.Join(dir, "b.tex")); err != nil || listing.Path != dir {
		t.Fatalf("file: %s, %v", listing.Path, err)
	}
	// Empty means home.
	if listing, err = listFolder(""); err != nil || listing.Path != homeDir() {
		t.Fatalf("home: %s, %v", listing.Path, err)
	}
	// The top has no parent.
	top := filepath.VolumeName(dir) + string(filepath.Separator)
	if listing, err = listFolder(top); err != nil || listing.Parent != "" {
		t.Fatalf("top: %+v, %v", listing, err)
	}
	for _, bad := range []string{"relative", "./x", "a\x00b"} {
		if _, err := listFolder(bad); err == nil {
			t.Errorf("%q was listed", bad)
		}
	}
}

func TestPickedPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.tex"), "")
	if p, err := pickedSavePath(filepath.Join(dir, "x", "..", "b.tex")); err != nil || p != filepath.Join(dir, "b.tex") {
		t.Fatalf("cleaned: %s %v", p, err)
	}
	for _, bad := range []string{"b.tex", "", dir, filepath.Join(dir, "missing", "b.tex")} {
		if _, err := pickedSavePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAppPickFlow(t *testing.T) {
	isolate(t)
	rec := &recorder{}
	app := newApp(rec.emit)
	dir := t.TempDir()

	app.Handle([]byte(`{"cmd":"save","contents":"hello"}`))
	if ev := rec.last(t); ev["type"] != "pick" || ev["mode"] != "save_tex" || ev["suggested_name"] != "main.tex" {
		t.Fatalf("untitled save: %v", ev)
	}
	target := filepath.Join(dir, "doc.tex")
	app.Handle(mustJSON(map[string]string{"cmd": "save", "contents": "hello", "path": target}))
	if ev := rec.last(t); ev["type"] != "document_saved" || ev["workspace_root"] != dir || ev["preview_url"] != nil {
		t.Fatalf("saved: %v", ev)
	}
	// Saved once, Save writes the same file without asking.
	app.Handle([]byte(`{"cmd":"save","contents":"again"}`))
	if data, _ := os.ReadFile(target); string(data) != "again" {
		t.Fatalf("contents %q", data)
	}
	// Save As always asks, suggesting the current name.
	app.Handle([]byte(`{"cmd":"save_as","contents":"x"}`))
	if ev := rec.last(t); ev["type"] != "pick" || ev["suggested_name"] != "doc.tex" || ev["start_dir"] != dir {
		t.Fatalf("save as: %v", ev)
	}
	app.Handle([]byte(`{"cmd":"open_file","path":"relative.tex"}`))
	if ev := rec.last(t); ev["type"] != "status" {
		t.Fatalf("relative open: %v", ev)
	}
	app.Handle([]byte(`{"cmd":"bogus"}`))
	if ev := rec.last(t); ev["type"] != "status" || ev["detail"] != "invalid IPC payload" {
		t.Fatalf("bogus: %v", ev)
	}
	app.Handle([]byte(`{"cmd":"run"}`))
	if ev := rec.last(t); ev["type"] != "status" || ev["detail"] != "invalid IPC payload" {
		t.Fatalf("run without contents: %v", ev)
	}
}
