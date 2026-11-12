// submini — a tiny same-origin host for the iPad mini (iOS 9) Subsonic player.
//
// It does exactly two things:
//   1. Serve the single-file ES5 player page (web/mini.html, embedded).
//   2. Reverse-proxy /rest/* to a Subsonic API server (Navidrome by default),
//      so the old Safari sees page + API as one origin (no CORS) and stream
//      Range requests/responses pass straight through.
//
// Configuration is environment-only:
//
//	SUBSONIC_URL  upstream base URL (default http://localhost:4533)
//	LISTEN        listen address    (default :8080)
package main

import (
	_ "embed"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed web/mini.html
var playerPage []byte

func main() {
	upstream := strings.TrimRight(os.Getenv("SUBSONIC_URL"), "/")
	if upstream == "" {
		upstream = "http://localhost:4533"
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("submini: bad SUBSONIC_URL %q", upstream)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Stream audio without buffering pauses: flush as data arrives.
	proxy.FlushInterval = -1
	proxy.Transport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	baseError := proxy.ErrorHandler
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("submini: proxy %s %s: %v", r.Method, r.URL.Path, err)
		baseError(w, r, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/rest/", proxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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

	log.Printf("submini: listening on %s, upstream %s", listen, upstream)
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
