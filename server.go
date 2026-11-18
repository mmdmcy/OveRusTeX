package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

const maxBodyBytes = 64 << 20

// server serves the editor page on 127.0.0.1 to one browser. Every request
// needs the startup token, a Host of exactly this address and port, and no
// Origin but its own; the page gets events over /events (server-sent
// events), sends commands to /ipc and lists folders for the picker at
// /api/list.
type server struct {
	app        *App
	token      string
	port       int
	cookieName string
	csp        string
	idle       time.Duration

	mu       sync.Mutex
	clients  map[chan []byte]struct{}
	lastSeen time.Time

	done     chan struct{}
	stopOnce sync.Once
	ln       net.Listener
	http     *http.Server
}

// newServer listens on 127.0.0.1:port (0 picks a free port). The program
// stops once no page has been connected for idle.
func newServer(port int, idle time.Duration) (*server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		ln.Close()
		return nil, err
	}
	s := &server{
		token:    hex.EncodeToString(raw),
		port:     ln.Addr().(*net.TCPAddr).Port,
		csp:      contentSecurityPolicy(uiHTML),
		idle:     idle,
		clients:  map[chan []byte]struct{}{},
		lastSeen: time.Now(),
		done:     make(chan struct{}),
		ln:       ln,
	}
	// Cookies are per host, not per port, so the port is in the name.
	s.cookieName = fmt.Sprintf("overustex_%d", s.port)
	s.app = newApp(s.broadcast)
	s.http = &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

func (s *server) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/?token=%s", s.port, s.token)
}

// contentSecurityPolicy allows the page's one inline script by its hash and
// nothing from elsewhere; the preview is a blob: frame.
func contentSecurityPolicy(page []byte) string {
	html := string(page)
	start := strings.Index(html, "<script>")
	end := strings.LastIndex(html, "</script>")
	script := ""
	if start >= 0 && end > start {
		script = html[start+len("<script>") : end]
	}
	sum := sha256.Sum256([]byte(script))
	return "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' blob: data:; " +
		"frame-src blob:; object-src blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /index.html", s.servePage)
	mux.HandleFunc("GET /events", s.serveEvents)
	mux.HandleFunc("POST /ipc", s.serveIPC)
	mux.HandleFunc("POST /api/list", s.serveList)
	mux.HandleFunc("GET /preview.pdf", s.servePreview)
	return s.guard(mux)
}

func refuse(w http.ResponseWriter, why string) {
	http.Error(w, "refused: "+why, http.StatusForbidden)
}

// guard runs before every handler: Host, Origin, Sec-Fetch-Site, then the
// token (in the URL once, a cookie after that).
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != fmt.Sprintf("127.0.0.1:%d", s.port) && r.Host != fmt.Sprintf("localhost:%d", s.port) {
			refuse(w, "unexpected Host header")
			return
		}
		if origin, ok := r.Header["Origin"]; ok && (len(origin) != 1 || origin[0] != "http://"+r.Host) {
			refuse(w, "foreign Origin")
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			refuse(w, "cross-site request")
			return
		}
		if token := r.URL.Query().Get("token"); token != "" && r.URL.Path == "/" {
			if !s.tokenOK(token) {
				refuse(w, "wrong token")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: s.cookieName, Value: s.token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(s.cookieName)
		if err != nil || !s.tokenOK(cookie.Value) {
			refuse(w, "missing or wrong token; open the address OverusTeX printed")
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *server) servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", s.csp)
	w.Write(uiHTML)
}

func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

func (s *server) serveIPC(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r) {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var probe struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Cmd == "quit" {
		s.broadcast(statusEvent{Type: "status", Message: "OverusTeX stopped", Detail: "You can close this tab."})
		w.WriteHeader(http.StatusNoContent)
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.stop()
		}()
		return
	}
	s.app.Handle(body)
	w.WriteHeader(http.StatusNoContent)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return io.ReadAll(r.Body)
}

func (s *server) serveList(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r) {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	body, err := readBody(w, r)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	listing, err := listFolder(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(listing)
}

// servePreview serves the newest preview PDF, with byte ranges.
func (s *server) servePreview(w http.ResponseWriter, r *http.Request) {
	path := s.app.PreviewPDF()
	if path == "" {
		http.Error(w, "Preview PDF unavailable", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Preview PDF not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="preview.pdf"`)
	http.ServeContent(w, r, "preview.pdf", info.ModTime(), f)
}

// serveEvents streams events to one page. An open stream is also how the
// server knows a page is still there.
func (s *server) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 64)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.lastSeen = time.Now()
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
}

// broadcast sends an event to every open page, in order.
func (s *server) broadcast(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintln(os.Stderr, "event encoding error:", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- data:
		default: // a page that stopped reading misses events instead of blocking the editor
		}
	}
}

func (s *server) connected() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients), s.lastSeen
}

func (s *server) stop() { s.stopOnce.Do(func() { close(s.done) }) }

// watchIdle stops the server once no page has been connected for s.idle.
func (s *server) watchIdle() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			if n, last := s.connected(); n == 0 && time.Since(last) > s.idle {
				s.stop()
				return
			}
		}
	}
}

// Serve runs until Quit, idle or ctx ends.
func (s *server) Serve(ctx context.Context) error {
	go s.watchIdle()
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()
	select {
	case err := <-errc:
		return err
	case <-s.done:
	case <-ctx.Done():
		s.stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.http.Shutdown(shutdown)
}
