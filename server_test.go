package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type liveServer struct {
	s      *server
	base   string // http://127.0.0.1:PORT
	cookie *http.Cookie
	client *http.Client
}

// startServer runs a real server on a free port until the test ends.
func startServer(t *testing.T, idle time.Duration) *liveServer {
	t.Helper()
	isolate(t)
	s, err := newServer(0, idle)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; s.app.builds.Wait() })
	ls := &liveServer{
		s:    s,
		base: fmt.Sprintf("http://127.0.0.1:%d", s.port),
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	ls.cookie = &http.Cookie{Name: s.cookieName, Value: s.token}
	return ls
}

func (ls *liveServer) do(t *testing.T, method, path string, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ls.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(ls.cookie)
	if mutate != nil {
		mutate(req)
	}
	resp, err := ls.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTokenIsRequired(t *testing.T) {
	ls := startServer(t, time.Minute)
	noCookie := func(r *http.Request) { r.Header.Del("Cookie") }
	for _, path := range []string{"/", "/index.html", "/events", "/preview.pdf"} {
		if resp := ls.do(t, "GET", path, "", noCookie); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s without token: %d", path, resp.StatusCode)
		}
	}
	if resp := ls.do(t, "POST", "/ipc", `{"cmd":"ready"}`, noCookie); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /ipc without token: %d", resp.StatusCode)
	}
	wrongCookie := func(r *http.Request) {
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: ls.s.cookieName, Value: strings.Repeat("0", 64)})
	}
	if resp := ls.do(t, "GET", "/", "", wrongCookie); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong cookie: %d", resp.StatusCode)
	}
	if resp := ls.do(t, "GET", "/?token=nope", "", noCookie); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong URL token: %d", resp.StatusCode)
	}
	// The token in another path does not count.
	if resp := ls.do(t, "POST", "/ipc?token="+ls.s.token, `{"cmd":"ready"}`, noCookie); resp.StatusCode != http.StatusForbidden {
		t.Errorf("URL token on /ipc: %d", resp.StatusCode)
	}
}

func TestTokenURLSetsStrictCookie(t *testing.T) {
	ls := startServer(t, time.Minute)
	resp := ls.do(t, "GET", "/?token="+ls.s.token, "", func(r *http.Request) { r.Header.Del("Cookie") })
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	c := cookies[0]
	if c.Name != ls.s.cookieName || c.Value != ls.s.token || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie %+v", c)
	}
	page := ls.do(t, "GET", "/", "", nil)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page %d", page.StatusCode)
	}
	csp := page.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP %q", csp)
	}
	if page.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header sent")
	}
}

func TestHostHeaderIsChecked(t *testing.T) {
	ls := startServer(t, time.Minute)
	for _, host := range []string{"evil.example", "evil.example:" + fmt.Sprint(ls.s.port), "127.0.0.1", "127.0.0.1:1", "localhost"} {
		resp := ls.do(t, "GET", "/", "", func(r *http.Request) { r.Host = host })
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: %d", host, resp.StatusCode)
		}
	}
	resp := ls.do(t, "GET", "/", "", func(r *http.Request) { r.Host = fmt.Sprintf("localhost:%d", ls.s.port) })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("localhost:PORT: %d", resp.StatusCode)
	}
}

func TestForeignOriginIsRefused(t *testing.T) {
	ls := startServer(t, time.Minute)
	for _, origin := range []string{"https://evil.example", "null", "http://127.0.0.1:1", "http://localhost:" + fmt.Sprint(ls.s.port)} {
		resp := ls.do(t, "POST", "/ipc", `{"cmd":"new_document"}`, func(r *http.Request) { r.Header.Set("Origin", origin) })
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: %d", origin, resp.StatusCode)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("CORS header for %q", origin)
		}
	}
	for _, site := range []string{"cross-site", "same-site"} {
		resp := ls.do(t, "GET", "/preview.pdf", "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", site) })
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %s: %d", site, resp.StatusCode)
		}
	}
	resp := ls.do(t, "POST", "/ipc", `{"cmd":"new_document"}`, func(r *http.Request) {
		r.Header.Set("Origin", ls.base)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("own origin: %d", resp.StatusCode)
	}
	// A cross-site form can only send text/plain or form data.
	if resp := ls.do(t, "POST", "/ipc", "", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", resp.StatusCode)
	}
}

// events opens the event stream and returns a function that reads the next
// event of the given type.
func (ls *liveServer) events(t *testing.T) func(typ string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ls.base+"/events", nil)
	req.AddCookie(ls.cookie)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := ls.client.Do(req.WithContext(ctx))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", resp, err)
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- data
			}
		}
		close(lines)
	}()
	waitConnected(t, ls.s, 1)
	return func(typ string) map[string]any {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %s", typ)
				}
				var ev map[string]any
				json.Unmarshal([]byte(line), &ev)
				if ev["type"] == typ {
					return ev
				}
			case <-timeout:
				t.Fatalf("no %s event", typ)
			}
		}
	}
}

func waitConnected(t *testing.T, s *server, n int) {
	t.Helper()
	for range 500 {
		if got, _ := s.connected(); got == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d connected pages", n)
}

func TestEditorFlowOverHTTP(t *testing.T) {
	ls := startServer(t, time.Minute)
	setupStandins(t, "", "pdflatex")
	next := ls.events(t)

	ls.do(t, "POST", "/ipc", `{"cmd":"ready"}`, nil)
	ev := next("document_loaded")
	if ev["display_path"] != "Untitled" || ev["contents"] != defaultTemplate || ev["preview_url"] != nil {
		t.Fatalf("ready: %v", ev)
	}

	folder := filepath.Join(t.TempDir(), "paper")
	writeFile(t, filepath.Join(folder, "main.tex"), "main")
	writeFile(t, filepath.Join(folder, "parts", "one.tex"), "one")

	ls.do(t, "POST", "/ipc", `{"cmd":"open_folder"}`, nil)
	pick := next("pick")
	if pick["mode"] != "open_folder" || pick["cmd"] != "open_folder" || pick["start_dir"] != ls.s.app.workspaceRoot {
		t.Fatalf("pick %v", pick)
	}
	ls.do(t, "POST", "/ipc", string(mustJSON(map[string]string{"cmd": "open_folder", "path": folder})), nil)
	ev = next("document_loaded")
	if ev["contents"] != "main" || ev["workspace_root"] != folder {
		t.Fatalf("open folder: %v", ev)
	}

	ls.do(t, "POST", "/ipc", `{"cmd":"open_workspace_file","path":"parts/one.tex"}`, nil)
	if ev = next("document_loaded"); ev["contents"] != "one" || ev["workspace_root"] != folder {
		t.Fatalf("workspace file: %v", ev)
	}
	ls.do(t, "POST", "/ipc", `{"cmd":"open_workspace_file","path":"../../etc/passwd"}`, nil)
	if ev = next("status"); ev["message"] != "Something went wrong" || !strings.Contains(ev["log"].(string), "not inside the workspace") {
		t.Fatalf("escape: %v", ev)
	}

	ls.do(t, "POST", "/ipc", `{"cmd":"run","contents":"edited"}`, nil)
	if ev = next("build_started"); ev["log"] != "Building one.tex as an internal working copy ..." {
		t.Fatalf("started: %v", ev)
	}
	ev = next("build_finished")
	if ev["success"] != true || !strings.HasPrefix(ev["preview_url"].(string), "/preview.pdf?ts=") {
		t.Fatalf("finished: %v", ev)
	}
	if data, _ := os.ReadFile(filepath.Join(folder, "parts", "one.tex")); string(data) != "one" {
		t.Fatal("Run saved the file")
	}

	pdf := ls.do(t, "GET", "/preview.pdf?ts=1", "", nil)
	body, _ := io.ReadAll(pdf.Body)
	if pdf.StatusCode != http.StatusOK || pdf.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(string(body), "%PDF") {
		t.Fatalf("preview %d %q", pdf.StatusCode, body)
	}
	part := ls.do(t, "GET", "/preview.pdf", "", func(r *http.Request) { r.Header.Set("Range", "bytes=0-3") })
	body, _ = io.ReadAll(part.Body)
	if part.StatusCode != http.StatusPartialContent || string(body) != "%PDF" {
		t.Fatalf("range %d %q", part.StatusCode, body)
	}

	ls.do(t, "POST", "/ipc", `{"cmd":"save_pdf_as","contents":"x"}`, nil)
	pick = next("pick")
	if pick["mode"] != "save_pdf" || pick["suggested_name"] != "export.pdf" || pick["start_dir"] != folder {
		t.Fatalf("pick %v", pick)
	}
	target := filepath.Join(folder, "out", "final.pdf")
	os.Mkdir(filepath.Dir(target), 0o755)
	ls.do(t, "POST", "/ipc", string(mustJSON(map[string]string{"cmd": "save_pdf_as", "contents": "x", "path": target})), nil)
	if ev = next("build_finished"); ev["exported_pdf_path"] != target {
		t.Fatalf("save pdf as: %v", ev)
	}

	ls.do(t, "POST", "/ipc", `{"cmd":"save","contents":"saved"}`, nil)
	if ev = next("document_saved"); ev["preview_label"] != "rendered" {
		t.Fatalf("save: %v", ev)
	}
	if data, _ := os.ReadFile(filepath.Join(folder, "parts", "one.tex")); string(data) != "saved" {
		t.Fatal("Save did not write the file")
	}
}

func TestListEndpoint(t *testing.T) {
	ls := startServer(t, time.Minute)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.tex"), "")
	resp := ls.do(t, "POST", "/api/list", string(mustJSON(map[string]string{"path": dir})), nil)
	var listing pickerListing
	json.NewDecoder(resp.Body).Decode(&listing)
	if resp.StatusCode != http.StatusOK || listing.Path != dir || len(listing.Entries) != 1 {
		t.Fatalf("%d %+v", resp.StatusCode, listing)
	}
	if resp := ls.do(t, "POST", "/api/list", `{"path":"relative/dir"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("relative path: %d", resp.StatusCode)
	}
}

func TestQuitAndIdleStop(t *testing.T) {
	ls := startServer(t, time.Minute)
	ls.events(t)
	ls.do(t, "POST", "/ipc", `{"cmd":"quit"}`, nil)
	select {
	case <-ls.s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not stop the server")
	}

	// A connected page keeps the server running past its idle time.
	idle := startServer(t, 300*time.Millisecond)
	idle.events(t)
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-idle.s.done:
		t.Fatal("stopped while a page was connected")
	default:
	}
	// With no page at all it stops.
	lone := startServer(t, 300*time.Millisecond)
	select {
	case <-lone.s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle server did not stop")
	}
}
