package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/vram"
)

const maxBodyBytes = 32 << 20 // §9.1

// LogSource — хвост вывода llama-server (реализует *supervisor.Supervisor).
type LogSource interface {
	Logs(model string, tail int) (lines []string, ok bool)
}

type route struct {
	method  string
	handler http.HandlerFunc
}

type server struct {
	cfg         *config.Config // конфиг старта: только глобальные поля (swap, llama, server, version)
	live        *config.Live   // модели: один live.Load() на проверку имени (§4.5 v2)
	sched       *scheduler.Scheduler
	logs        LogSource
	probe       vram.Probe
	keys        [][]byte
	log         *slog.Logger
	started     time.Time
	routes      map[string]route
	afterGrant  func(r *http.Request) // тестовый шов G13; в хосте nil
	ctl         *control.Controller   // nil — маршрутов /api/admin/* нет (WithAdmin не вызван)
	auth        *control.Auth
	hosts       map[string]bool                    // допустимые Host и Origin /api/admin/* (§9.1 v2)
	adminRoutes map[string]map[string]adminHandler // путь → метод → обработчик
	now         func() time.Time                   // часы входа и сессий (§9.4); тесты подменяют
	headers     HeaderCache                        // nil — meta.n_ctx_train и meta.n_embd равны 0 (WithHeaders не вызван)
	panel       fs.FS                              // файлы панели /ui (WithAdmin: ui.Files()); nil — /ui как в MVP
}

// Option — настройка шлюза при создании (WithAdmin, WithHeaders).
type Option func(*server)

// New — HTTP-обработчик шлюза (§9). Глобальные поля берутся из live.Load() при создании, модели — из live
// на каждый запрос. Без WithAdmin пути /api/admin/* и /ui обрабатываются как в MVP (ключ клиента, затем 404).
func New(live *config.Live, s *scheduler.Scheduler, logs LogSource, p vram.Probe, keys [][]byte, log *slog.Logger, opts ...Option) http.Handler {
	srv := newServer(live, s, logs, p, keys, log)
	for _, o := range opts {
		o(srv)
	}
	return srv
}

func newServer(live *config.Live, s *scheduler.Scheduler, logs LogSource, p vram.Probe, keys [][]byte, log *slog.Logger) *server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := &server{cfg: live.Load(), live: live, sched: s, logs: logs, probe: p, keys: keys, log: log, started: time.Now(),
		now: time.Now}
	srv.routes = srv.fixedRoutes()
	return srv
}

// fixedRoutes — точные пути §9.2 (кроме префикса POST /v1/) и GET /props (F5-D1).
func (s *server) fixedRoutes() map[string]route {
	return map[string]route{
		"/health":        {http.MethodGet, s.health},
		"/v1/models":     {http.MethodGet, s.v1Models},
		"/models":        {http.MethodGet, s.models},
		"/models/load":   {http.MethodPost, s.modelsLoad},
		"/models/unload": {http.MethodPost, s.modelsUnload},
		"/props":         {http.MethodGet, s.props},
		"/api/status":    {http.MethodGet, s.apiStatus},
		"/api/swap":      {http.MethodPost, s.apiSwap},
		"/api/unload":    {http.MethodPost, s.apiUnload},
		"/api/logs":      {http.MethodGet, s.apiLogs},
	}
}

// NewHTTPServer — §9.1: ReadTimeout и WriteTimeout 0 (долгий prefill, стримы, ожидание свапа).
func NewHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		DisableGeneralOptionsHandler: true, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
}

// reqInfo — поля строки журнала доступа, которые заполняют обработчики.
type reqInfo struct {
	model  string
	wait   time.Duration
	waited bool
}

type infoKey struct{}

func reqInfoOf(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(infoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

func noteModel(r *http.Request, model string) { reqInfoOf(r).model = model }

func noteWait(r *http.Request, d time.Duration) {
	ri := reqInfoOf(r)
	ri.wait, ri.waited = d, true
}

// accessWriter считает статус и байты; не буферизует; Unwrap/Flush доводят ResponseController до исходного writer (§9.1).
type accessWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *accessWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ServeHTTP — маршрутизация и строка журнала доступа (§13). В журнал не попадают query, тело и заголовки,
// кроме Tailscale-User-Login.
//
// Строка доступа пишется в defer, а не после route: при обрыве стрима клиентом (или при Retire поколения
// посреди ответа) httputil.ReverseProxy под настоящим http.Server паникует http.ErrAbortHandler
// (shouldPanicOnCopyError), и обычный код после route не выполнился бы — строка `status=499` §13
// никогда бы не появилась (тест G5). defer перехватывает ровно эту панику, пишет строку и паникует снова,
// чтобы net/http как обычно закрыл соединение без трассировки. Любая другая паника пробрасывается сразу.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ri := &reqInfo{}
	r = r.WithContext(context.WithValue(r.Context(), infoKey{}, ri))
	aw := &accessWriter{ResponseWriter: w}
	defer func() {
		p := recover()
		if p != nil && p != http.ErrAbortHandler {
			panic(p)
		}
		status := aw.status
		if status == 0 {
			status = http.StatusOK
		}
		if r.Context().Err() != nil || p == http.ErrAbortHandler {
			status = 499 // клиент оборвал связь (§13)
		}
		attrs := []slog.Attr{slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Int64("bytes_out", aw.bytes),
			slog.String("remote", r.RemoteAddr)}
		if ri.model != "" {
			attrs = append(attrs, slog.String("model", ri.model))
		}
		if ri.waited {
			attrs = append(attrs, slog.Int64("wait_ms", ri.wait.Milliseconds()))
		}
		if u := r.Header.Get("Tailscale-User-Login"); u != "" {
			attrs = append(attrs, slog.String("ts_user", u))
		}
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
		if p != nil {
			panic(p) // http.ErrAbortHandler: net/http закрывает соединение как обычно
		}
	}()
	s.route(aw, r)
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		s.health(w, r)
		return
	}
	if s.ctl != nil && isUIPath(r.URL.Path) {
		s.serveUI(w, r) // §9.1 v2 п. 2: Host, затем файлы панели без ключа
		return
	}
	if s.ctl != nil && strings.HasPrefix(r.URL.Path, "/api/admin/") {
		s.admin(w, r) // §9.1 v2 п. 3: Host и Origin, вход, Bearer администратора — без ключа клиента
		return
	}
	if !s.authorized(r) {
		s.log.Warn("invalid api key", "remote", r.RemoteAddr, "ts_user", r.Header.Get("Tailscale-User-Login"), "path", r.URL.Path)
		writeAPIError(w, errUnauthorized())
		return
	}
	if hasDotSegment(r.URL.Path) || hasDotSegment(r.URL.EscapedPath()) {
		writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		return
	}
	if rt, ok := s.routes[r.URL.Path]; ok {
		if r.Method != rt.method {
			writeAPIError(w, errMethodNotAllowed(r.Method, r.URL.Path, rt.method))
			return
		}
		rt.handler(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		switch r.Method {
		case http.MethodPost:
			s.v1Proxy(w, r)
		case http.MethodGet:
			writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		default:
			writeAPIError(w, errMethodNotAllowed(r.Method, r.URL.Path, http.MethodPost))
		}
		return
	}
	writeAPIError(w, errNotFound(r.Method, r.URL.Path))
}

// hasDotSegment проверяет путь без его изменения, включая закодированные точки и разделители.
func hasDotSegment(path string) bool {
	path = strings.NewReplacer("%2e", ".", "%2E", ".", "%2f", "/", "%2F", "/").Replace(path)
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// authorized — только Authorization: Bearer <ключ>; сравнение за постоянное время со всеми ключами (§9.1).
func (s *server) authorized(r *http.Request) bool {
	got := []byte(bearer(r))
	if len(got) == 0 {
		return false
	}
	match := 0
	for _, k := range s.keys {
		match |= subtle.ConstantTimeCompare(got, k)
	}
	return match == 1
}

// bearer — значение Authorization: Bearer <значение> без пробелов по краям; схема без учёта регистра. "" — заголовка
// нет или схема другая. Значение не пишется в журнал.
func bearer(r *http.Request) string {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON — ответ хоста (§9.1); HTML-экранирование выключено, как в writeAPIError.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// readBody — тело целиком, не больше 32 МиБ; сжатое тело — 400 (§9.1).
func (s *server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		writeAPIError(w, errInvalidRequest())
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAPIError(w, errTooLarge())
		} else {
			writeAPIError(w, errInvalidRequest())
		}
		return nil, false
	}
	return body, true
}

// modelOf — верхнеуровневое строковое поле model (§9.3 п. 2).
func modelOf(body []byte) (string, bool) {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Model == "" {
		return "", false
	}
	return v.Model, true
}

// requestModel читает тело и поле model; при ошибке ответ уже записан (400, 404, 413).
func (s *server) requestModel(w http.ResponseWriter, r *http.Request) (model string, body []byte, ok bool) {
	body, ok = s.readBody(w, r)
	if !ok {
		return "", nil, false
	}
	model, ok = modelOf(body)
	if !ok {
		writeAPIError(w, errInvalidRequest())
		return "", nil, false
	}
	cfg := s.live.Load()
	if _, known := cfg.Model(model); !known {
		writeAPIError(w, errModelNotFound(model, cfg.ModelIDs()))
		return "", nil, false
	}
	noteModel(r, model) // только имя из конфига: произвольная строка клиента в журнал не попадает
	return model, body, true
}
