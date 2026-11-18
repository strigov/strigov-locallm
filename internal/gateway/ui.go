package gateway

import (
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// uiCSP — Content-Security-Policy панели (§9.1).
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiTypes — Content-Type файлов панели по расширению; другое расширение — 404.
var uiTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

// isUIPath — путь /ui или /ui/… (§9.1 v2 п. 2).
func isUIPath(p string) bool {
	return p == "/ui" || strings.HasPrefix(p, "/ui/")
}

// serveUI — панель (§9.1 v2 п. 2, §10.1): заголовки безопасности и CSP на любом ответе; Host — по списку
// /api/admin/* (Origin не проверяется); только GET; /ui → 308 на /ui/ с тем же query; затем файл плоского каталога
// s.panel без ключа (Authorization не смотрится). Журнал доступа пишет только путь: код из ?otp= в него не попадает.
func (s *server) serveUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("Cache-Control", "no-cache")
	if !s.hostAllowed(r.Host) {
		bad := clip100(r.Host)
		s.log.Warn("admin host rejected", "host", bad, "path", r.URL.Path)
		writeAPIError(w, errHostNotAllowed(bad))
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, errMethodNotAllowed(r.Method, r.URL.Path, http.MethodGet))
		return
	}
	if r.URL.Path == "/ui" {
		target := "/ui/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		h.Set("Location", target)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" {
		name = "index.html"
	}
	ctype, ok := uiTypes[path.Ext(name)]
	if !ok || s.panel == nil || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") || !fs.ValidPath(name) {
		writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		return
	}
	data, err := fs.ReadFile(s.panel, name)
	if err != nil {
		writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		return
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
