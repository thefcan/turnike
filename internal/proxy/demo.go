package proxy

import (
	_ "embed"
	"net/http"
	"strconv"
)

// demoPageHTML is the self-contained page served at "/" when
// server.demo_page is set. It is embedded rather than read from disk so
// the deploy image stays a single binary plus its config, and so the
// page can never go missing at runtime.
//
// Self-contained is a requirement, not a style choice: the page must
// work with no external origin to fetch from, and it drives the
// gateway's own routes, which only a same-origin script can read a
// status code from.
//
//go:embed demo.html
var demoPageHTML []byte

// serveDemoPage writes the embedded page. Callers are responsible for
// the method guard; net/http drops the body for HEAD on its own, so the
// Content-Length still advertises the real size.
func serveDemoPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(demoPageHTML)))
	// The page ships with the binary, so it is exactly as fresh as the
	// running deploy - but a cached copy pointing at a since-changed
	// route table would demo the wrong thing, and it is one small file.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(demoPageHTML)
}
