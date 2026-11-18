package gateway

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/strigov/strigov-locallm/internal/config"
)

// allowedHosts — допустимые Host и Origin /api/admin/* (§9.1 v2) в записи config.NormalizeHost: 127.0.0.1, ::1,
// localhost, хост server.listen и server.admin_hosts. Хост 0.0.0.0 или :: из server.listen не добавляется: это «все
// адреса», а не имя, по которому открывают панель. Имена на .ts.net проверяет hostAllowed.
func allowedHosts(cfg *config.Config) map[string]bool {
	hosts := map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}
	if h, _, err := net.SplitHostPort(cfg.Server.Listen); err == nil {
		if a, err := netip.ParseAddr(h); err != nil || !a.IsUnspecified() {
			if n, ok := config.NormalizeHost(h); ok {
				hosts[n] = true
			}
		}
	}
	for _, h := range cfg.Server.AdminHosts {
		if n, ok := config.NormalizeHost(h); ok {
			hosts[n] = true
		}
	}
	return hosts
}

// hostOnly — хост из значения Host или из хоста URL: без порта, без скобок IPv6, без точки в конце имени.
// Незакрытая скобка — "" (отказ).
func hostOnly(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	} else if inner, ok := strings.CutPrefix(hostport, "["); ok {
		if h, ok = strings.CutSuffix(inner, "]"); !ok {
			return ""
		}
	}
	return strings.TrimSuffix(h, ".")
}

// hostAllowed — хост из списка allowedHosts или имя на .ts.net; сравнение после config.NormalizeHost (нижний
// регистр, IPv6 в записи netip, F2-D1 ledger первого плана).
func (s *server) hostAllowed(hostport string) bool {
	n, ok := config.NormalizeHost(hostOnly(hostport))
	return ok && (s.hosts[n] || strings.HasSuffix(n, ".ts.net"))
}

// hostOK — Host и Origin запроса к /api/admin/* (§9.1 v2). Origin, если он есть, — один, со схемой http или https
// и допустимым хостом; "null" — отказ. Отказ — значение, которое не прошло, не длиннее 100 символов (для текста
// ошибки и журнала).
func (s *server) hostOK(r *http.Request) (bad string, ok bool) {
	if !s.hostAllowed(r.Host) {
		return clip100(r.Host), false
	}
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return "", true
	case 1:
	default:
		return clip100(strings.Join(origins, ", ")), false
	}
	u, err := url.Parse(origins[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !s.hostAllowed(u.Host) {
		return clip100(origins[0]), false
	}
	return "", true
}

// clip100 — s как верный UTF-8, не длиннее 100 символов.
func clip100(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= 100 {
		return s
	}
	return string([]rune(s)[:100])
}
