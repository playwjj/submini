// submini — a tiny host for the iPad mini (iOS 9) Subsonic player.
//
// It serves the single-file ES5 player page (web/mini.html, embedded) and
// reverse-proxies /rest/* to the Subsonic server the user configured in
// the web UI, so the old Safari sees page + API as one origin (no CORS)
// and stream Range requests pass straight through.
//
// The connection (server URL + username + Subsonic token) is configured
// once in the UI and kept in a small SQLite database; the proxy injects
// the auth parameters server-side, so the page never handles credentials
// beyond the one-time setup form. The raw password is never stored —
// only the md5 token and its salt, which is all the Subsonic API needs.
//
// Configuration is environment-only:
//
//	DATA_DIR  where submini.db lives (default ".", /data in the image)
//	LISTEN    listen address       (default :9898)
package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web/mini.html
var playerPage []byte

// Home-screen icons and iPad splash screens (artwork borrowed from
// subgram). Served under /icons/ plus the root paths iOS probes for.

//go:embed web/icons
var iconsFS embed.FS

// placeholderCover replaces cover art the iPad cannot show. Navidrome
// answers getCoverArt for an album without art with its own placeholder,
// a WebP image, and iOS 9 Safari cannot decode WebP at all; a missing
// cover (error status) gets the same treatment.
func placeholderCover(resp *http.Response) error {
	if !strings.HasSuffix(strings.TrimSuffix(resp.Request.URL.Path, ".view"), "/getCoverArt") {
		return nil
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if ok && !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/webp") {
		return nil
	}
	data, err := iconsFS.ReadFile("web/icons/cover-placeholder.jpg")
	if err != nil {
		return nil
	}
	_ = resp.Body.Close()
	resp.StatusCode = http.StatusOK
	resp.Status = "200 OK"
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	h := resp.Header
	for _, k := range []string{"Content-Encoding", "Content-Range", "Etag", "Last-Modified", "Transfer-Encoding"} {
		h.Del(k)
	}
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", fmt.Sprint(len(data)))
	return nil
}

// ---------------- cover cache ----------------
// Cover art is immutable per (id, size), but the random-songs view asks
// for ~50 fresh covers at once, most never requested before, while iOS 9
// Safari only opens a handful of connections per host — bursts trickled
// in (or looked broken) while Navidrome generated every resize live.
// Cache covers in memory after the WebP swap, and let the browser keep
// them for a week as well.
const (
	coverCacheMaxTotal = 32 << 20
	coverCacheMaxEntry = 2 << 20
)

type coverEntry struct {
	ctype string
	body  []byte
}

// version is stamped at release time via -ldflags -X main.version
// (the Dockerfile takes it from the git tag); local builds say "dev".
var version = "dev"

type coverStore struct {
	mu    sync.Mutex
	items map[string]*coverEntry
	order []string
	bytes int
	dir   string // disk layer under DATA_DIR/covers; "" disables it
}

var covers = &coverStore{items: make(map[string]*coverEntry)}

func (c *coverStore) get(key string) (*coverEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	return e, ok
}

func (c *coverStore) put(key string, e *coverEntry) {
	if len(e.body) == 0 || len(e.body) > coverCacheMaxEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.bytes -= len(old.body)
	} else {
		c.order = append(c.order, key)
	}
	c.items[key] = e
	c.bytes += len(e.body)
	for c.bytes > coverCacheMaxTotal && len(c.order) > 0 {
		k := c.order[0]
		c.order = c.order[1:]
		if old, ok := c.items[k]; ok {
			c.bytes -= len(old.body)
			delete(c.items, k)
		}
	}
}

func isCoverArt(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(path, ".view"), "/getCoverArt")
}

// The disk layer keeps covers across container restarts and upgrades:
// files are named by the SHA-256 of the cache key (the full upstream
// URL), so a lookup needs no index — and no index means nothing to
// corrupt. Content type is sniffed on read; only genuine covers are
// ever persisted (the caller decides), so placeholders stay unpinned.
const coverDiskMaxTotal = 64 << 20

func (c *coverStore) fileFor(key string) string {
	if c.dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".img")
}

func (c *coverStore) diskGet(key string) (*coverEntry, bool) {
	p := c.fileFor(key)
	if p == "" {
		return nil, false
	}
	body, err := os.ReadFile(p)
	if err != nil || len(body) == 0 || len(body) > coverCacheMaxEntry {
		return nil, false
	}
	return &coverEntry{ctype: http.DetectContentType(body), body: body}, true
}

func (c *coverStore) persist(key string, body []byte) {
	p := c.fileFor(key)
	if p == "" {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, body, 0o644) != nil {
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
		return
	}
	c.trimDisk()
}

func (c *coverStore) trimDisk() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type diskFile struct {
		path string
		size int64
		mod  time.Time
	}
	var files []diskFile
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".img") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
		files = append(files, diskFile{filepath.Join(c.dir, e.Name()), fi.Size(), fi.ModTime()})
	}
	if total <= coverDiskMaxTotal {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= coverDiskMaxTotal {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// coverTransport caches getCoverArt responses (after the placeholder
// swap, so the cached bytes are always ones the iPad can decode).
type coverTransport struct{ base http.RoundTripper }

func (t coverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !isCoverArt(req.URL.Path) || req.Header.Get("Range") != "" {
		return t.base.RoundTrip(req)
	}
	key := req.URL.String()
	if e, ok := covers.get(key); ok {
		return serveCachedCover(req, e), nil
	}
	if e, ok := covers.diskGet(key); ok {
		covers.put(key, e) // promote to memory
		return serveCachedCover(req, e), nil
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	// Was this already a decodable image before the swap? Only genuine
	// covers are cached — a swapped placeholder must not pin an album
	// that gains real art later to the cassette picture.
	ct := resp.Header.Get("Content-Type")
	genuine := resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "image/webp")
	// The same swap ModifyResponse performs; applying it here first
	// makes its second run a no-op.
	if err := placeholderCover(resp); err != nil {
		return resp, err
	}
	if genuine && resp.StatusCode == http.StatusOK {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, coverCacheMaxEntry+1))
		if rerr != nil {
			_ = resp.Body.Close()
			return nil, rerr
		}
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Cache-Control", "public, max-age=604800")
		covers.put(key, &coverEntry{ctype: resp.Header.Get("Content-Type"), body: body})
		covers.persist(key, body)
	}
	return resp, nil
}

func serveCachedCover(req *http.Request, e *coverEntry) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", e.ctype)
	h.Set("Content-Length", fmt.Sprint(len(e.body)))
	h.Set("Cache-Control", "public, max-age=604800")
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, ContentLength: int64(len(e.body)),
		Body:    io.NopCloser(bytes.NewReader(e.body)),
		Request: req,
	}
}

func serveIcon(w http.ResponseWriter, r *http.Request, name string) {
	data, err := iconsFS.ReadFile("web/icons/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "image/png"
	if strings.HasSuffix(name, ".jpg") {
		ctype = "image/jpeg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = w.Write(data)
}

type connConfig struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Token    string `json:"-"`
	Salt     string `json:"-"`
	base     *url.URL
}

/* ---------------- settings store (SQLite) ---------------- */

type store struct{ db *sql.DB }

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "submini.db"))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) load() (*connConfig, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v
	}
	if kv["server"] == "" || kv["username"] == "" || kv["token"] == "" {
		return nil, nil
	}
	base, err := url.Parse(kv["server"])
	if err != nil {
		return nil, nil
	}
	return &connConfig{Server: kv["server"], Username: kv["username"],
		Token: kv["token"], Salt: kv["salt"], base: base}, nil
}

func (s *store) save(cfg *connConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{
		"server": cfg.Server, "username": cfg.Username,
		"token": cfg.Token, "salt": cfg.Salt,
	} {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) clear() error {
	_, err := s.db.Exec(`DELETE FROM settings`)
	return err
}

/* ---------------- shared state ---------------- */

var (
	mu  sync.RWMutex
	cur *connConfig
	st  *store
)

func current() *connConfig {
	mu.RLock()
	defer mu.RUnlock()
	return cur
}

func setCurrent(cfg *connConfig) {
	mu.Lock()
	cur = cfg
	mu.Unlock()
}

/* ---------------- helpers ---------------- */

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomSalt() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func joinPaths(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

/* ---------------- API: connection setup ---------------- */

type connectRequest struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Shared outbound transport. The dialer is explicit so IPv6-only
// upstreams (e.g. a public AAAA-only hostname) work wherever the host
// or container network actually carries IPv6.
func outboundTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func pingSubsonic(cfg *connConfig) error {
	u := *cfg.base
	u.Path = joinPaths(u.Path, "/rest/ping.view")
	q := u.Query()
	q.Set("u", cfg.Username)
	q.Set("t", cfg.Token)
	q.Set("s", cfg.Salt)
	q.Set("v", "1.16.1")
	q.Set("c", "submini")
	q.Set("f", "json")
	u.RawQuery = q.Encode()
	client := &http.Client{Timeout: 12 * time.Second, Transport: outboundTransport()}
	resp, err := client.Get(u.String())
	if err != nil {
		return fmt.Errorf("连不上服务器：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Response struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("服务器响应不是 Subsonic API（HTTP %d）", resp.StatusCode)
	}
	if parsed.Response.Status != "ok" {
		msg := "用户名或密码错误"
		if parsed.Response.Error != nil && parsed.Response.Error.Message != "" {
			msg = parsed.Response.Error.Message
		}
		return fmt.Errorf("登录失败：%s", msg)
	}
	return nil
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req connectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式不对"})
		return
	}
	server := strings.TrimRight(strings.TrimSpace(req.Server), "/")
	base, err := url.Parse(server)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "服务器地址不对，应以 http:// 或 https:// 开头"})
		return
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户名和密码不能为空"})
		return
	}
	salt := randomSalt()
	cfg := &connConfig{
		Server:   server,
		Username: strings.TrimSpace(req.Username),
		Salt:     salt,
		Token:    md5hex(req.Password + salt),
		base:     base,
	}
	if err := pingSubsonic(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := st.save(cfg); err != nil {
		log.Printf("submini: save settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存设置失败"})
		return
	}
	setCurrent(cfg)
	log.Printf("submini: connected to %s as %s", cfg.Server, cfg.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "server": cfg.Server, "username": cfg.Username})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := current()
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true, "server": cfg.Server, "username": cfg.Username})
}

func handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if err := st.clear(); err != nil {
		log.Printf("submini: clear settings: %v", err)
	}
	setCurrent(nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- main ---------------- */

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":9898"
	}
	var err error
	st, err = openStore(dataDir)
	if err != nil {
		log.Fatalf("submini: open store: %v", err)
	}
	// Cover disk cache lives beside the settings DB; if it cannot be
	// created, the in-memory cache still works on its own.
	covers.dir = filepath.Join(dataDir, "covers")
	if err := os.MkdirAll(covers.dir, 0o755); err != nil {
		log.Printf("submini: cover disk cache off: %v", err)
		covers.dir = ""
	}
	cfg, err := st.load()
	if err != nil {
		log.Fatalf("submini: load settings: %v", err)
	}
	setCurrent(cfg)

	proxy := &httputil.ReverseProxy{
		// Stream audio without buffering pauses: flush as data arrives.
		FlushInterval:  -1,
		Transport:      coverTransport{base: outboundTransport()},
		ModifyResponse: placeholderCover,
		Director: func(r *http.Request) {
			c := current()
			target := c.base
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.URL.Path = joinPaths(target.Path, r.URL.Path)
			r.Host = target.Host
			q := r.URL.Query()
			// Auth is owned by the backend: drop whatever the page sent
			// and inject the stored token.
			q.Del("u")
			q.Del("t")
			q.Del("s")
			q.Del("p")
			q.Del("apiKey")
			q.Set("u", c.Username)
			q.Set("t", c.Token)
			q.Set("s", c.Salt)
			r.URL.RawQuery = q.Encode()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("submini: proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/", func(w http.ResponseWriter, r *http.Request) {
		if current() == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not configured"})
			return
		}
		proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/connect", handleConnect)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/screensaver", handleAPIScreensaver(dataDir))
	mux.HandleFunc("/screensaver/", handleSaverFile(dataDir))
	startSaverUpdater(dataDir)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{\"version\":%q}\n", version)
	})
	mux.HandleFunc("/api/disconnect", handleDisconnect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/icons/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/icons/")
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		serveIcon(w, r, name)
	})
	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		serveIcon(w, r, "apple-touch-icon-180.png")
	})
	mux.HandleFunc("/apple-touch-icon-precomposed.png", func(w http.ResponseWriter, r *http.Request) {
		serveIcon(w, r, "apple-touch-icon-180.png")
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		serveIcon(w, r, "favicon-32.png")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/mini.html" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(playerPage)
	})

	if cfg != nil {
		log.Printf("submini: listening on %s, configured for %s as %s", listen, cfg.Server, cfg.Username)
	} else {
		log.Printf("submini: listening on %s, not configured yet (open the page to set up)", listen)
	}
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
