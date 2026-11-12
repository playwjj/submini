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
	"crypto/md5"
	"crypto/rand"
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

func serveIcon(w http.ResponseWriter, r *http.Request, name string) {
	data, err := iconsFS.ReadFile("web/icons/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
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
	cfg, err := st.load()
	if err != nil {
		log.Fatalf("submini: load settings: %v", err)
	}
	setCurrent(cfg)

	proxy := &httputil.ReverseProxy{
		// Stream audio without buffering pauses: flush as data arrives.
		FlushInterval: -1,
		Transport:     outboundTransport(),
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
