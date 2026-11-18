package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// isolate gives the test its own home, cache, config and data folders, so it
// never reads or writes the real ones.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return home
}

// recorder collects the events an App emits.
type recorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (r *recorder) emit(ev any) {
	data, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	r.mu.Lock()
	r.events = append(r.events, m)
	r.mu.Unlock()
}

func (r *recorder) last(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		t.Fatal("no event emitted")
	}
	return r.events[len(r.events)-1]
}

func (r *recorder) waitFor(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, ev := range r.events {
			if ev["type"] == typ {
				r.mu.Unlock()
				return ev
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event", typ)
	return nil
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell stand-ins")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// fixedTime is an old, distinct modification time per index.
func fixedTime(i int) time.Time {
	return time.Date(2020, 1, 1, 0, 0, i, 0, time.UTC)
}
