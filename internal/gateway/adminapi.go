package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/ui"
)

const maxAdminBody = 1 << 20 // §9.1 v2: тело запроса администратора

// adminHandler — обработчик маршрута /api/admin/*; a — кто вызвал (Bearer уже проверен).
type adminHandler func(w http.ResponseWriter, r *http.Request, a control.Actor)

// WithAdmin подключает /api/admin/* и панель /ui (§9 спецификации v2): контроллер, вход и сессии, файлы ui.Files();
// допустимые Host — из конфига старта (server.listen и server.admin_hosts меняются только перезапуском).
func WithAdmin(ctl *control.Controller, auth *control.Auth) Option {
	return func(s *server) {
		s.ctl, s.auth = ctl, auth
		s.hosts = allowedHosts(s.cfg)
		s.adminRoutes = s.adminRouteTable()
		s.panel = ui.Files()
	}
}

// adminRouteTable — маршруты §9.2 v2.
func (s *server) adminRouteTable() map[string]map[string]adminHandler {
	// POST /api/admin/session сюда не доходит: вход обрабатывается до Bearer (admin). Запись нужна для Allow.
	login := func(w http.ResponseWriter, r *http.Request, _ control.Actor) { s.adminLogin(w, r) }
	return map[string]map[string]adminHandler{
		"/api/admin/session":        {http.MethodPost: login, http.MethodDelete: s.adminLogout},
		"/api/admin/state":          {http.MethodGet: s.adminGetState},
		"/api/admin/load":           {http.MethodPost: s.adminLoad},
		"/api/admin/unload":         {http.MethodPost: s.adminUnload},
		"/api/admin/restart":        {http.MethodPost: s.adminRestart},
		"/api/admin/pause":          {http.MethodPost: s.adminPause},
		"/api/admin/resume":         {http.MethodPost: s.adminResume},
		"/api/admin/models":         {http.MethodGet: s.adminGetModels, http.MethodPut: s.adminPutModels},
		"/api/admin/models/reload":  {http.MethodPost: s.adminReload},
		"/api/admin/models/migrate": {http.MethodPost: s.adminMigrate},
		"/api/admin/scan":           {http.MethodGet: s.adminScan},
		"/api/admin/logs":           {http.MethodGet: s.adminLogs},
	}
}

// admin — /api/admin/* по §9.1 v2 п. 3: заголовки безопасности на любом ответе, Host и Origin, вход без Bearer,
// Bearer администратора, точечные сегменты, маршрут, 404 или 405. Заголовков CORS нет; OPTIONS — 405.
func (s *server) admin(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	if bad, ok := s.hostOK(r); !ok {
		s.log.Warn("admin host rejected", "host", bad, "path", r.URL.Path)
		writeAPIError(w, errHostNotAllowed(bad))
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/admin/session" {
		s.adminLogin(w, r)
		return
	}
	got := bearer(r)
	actor, err := s.auth.Check(got, s.now())
	if err != nil {
		reason := "invalid bearer"
		switch {
		case errors.Is(err, control.ErrTooManyAttempts):
			reason = "locked"
		case got == "":
			reason = "missing bearer"
		}
		s.authFailed(r, reason)
		writeAPIError(w, authError(err))
		return
	}
	if hasDotSegment(r.URL.Path) || hasDotSegment(r.URL.EscapedPath()) {
		writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		return
	}
	methods, ok := s.adminRoutes[r.URL.Path]
	if !ok {
		writeAPIError(w, errNotFound(r.Method, r.URL.Path))
		return
	}
	handler, ok := methods[r.Method]
	if !ok {
		writeAPIError(w, errMethodNotAllowed(r.Method, r.URL.Path, strings.Join(slices.Sorted(maps.Keys(methods)), ", ")))
		return
	}
	handler(w, r, actor)
}

// authFailed — строка журнала неудачи (§13): адрес, пользователь Tailscale, путь, причина. Bearer, ключи и коды
// не пишутся.
func (s *server) authFailed(r *http.Request, reason string) {
	s.log.Warn("admin auth failed", "remote", r.RemoteAddr, "ts_user", r.Header.Get("Tailscale-User-Login"),
		"path", r.URL.Path, "reason", reason)
}

// adminBody разбирает тело POST и PUT в v (§9.1 v2): Content-Type application/json (параметры допустимы), без сжатия,
// не больше 1 МиБ, один JSON-объект без неизвестных полей; пустое тело — {}. false — ответ уже записан.
func (s *server) adminBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeAPIError(w, errUnsupportedMediaType())
		return false
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		writeAPIError(w, errAdminBody("compressed bodies are not accepted"))
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAPIError(w, errAdminTooLarge())
		} else {
			writeAPIError(w, errAdminBody("cannot read the body"))
		}
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	if data[0] != '{' {
		writeAPIError(w, errAdminBody("body must be a JSON object"))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeAPIError(w, errAdminBody(jsonDetail(err)))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeAPIError(w, errAdminBody("unexpected data after the JSON object"))
		return false
	}
	return true
}

// jsonDetail — деталь 400 без значений из тела: имя поля схемы или имя неизвестного ключа (не длиннее 100 символов).
func jsonDetail(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field == "" {
			return "body must be a JSON object"
		}
		return fmt.Sprintf("field %q has the wrong type", clip100(te.Field))
	}
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		return clip100(strings.TrimPrefix(msg, "json: "))
	}
	return "malformed JSON"
}

// adminLogin — POST /api/admin/session (§9.4): ровно одно из otp и admin_key; ответ — токен и срок.
func (s *server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		OTP      string `json:"otp"`
		AdminKey string `json:"admin_key"`
	}
	if !s.adminBody(w, r, &b) {
		return
	}
	if (b.OTP == "") == (b.AdminKey == "") {
		writeAPIError(w, errAdminBody(`exactly one of "otp" and "admin_key" is required`))
		return
	}
	token, expires, err := s.auth.Login(b.OTP, b.AdminKey, s.now())
	if err != nil {
		reason := "invalid admin key"
		switch {
		case errors.Is(err, control.ErrTooManyAttempts):
			reason = "locked"
		case b.OTP != "":
			reason = "invalid otp"
		}
		s.authFailed(r, reason)
		writeAPIError(w, authError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": expires.UTC().Format(time.RFC3339)})
}

// adminLogout — DELETE /api/admin/session: закрывает сессию токена; с ключом администратора ничего не меняет.
func (s *server) adminLogout(w http.ResponseWriter, r *http.Request, _ control.Actor) {
	s.auth.Logout(bearer(r))
	w.WriteHeader(http.StatusNoContent)
}

// adminGetState — GET /api/admin/state (§9.5 v2).
func (s *server) adminGetState(w http.ResponseWriter, r *http.Request, _ control.Actor) {
	writeJSON(w, http.StatusOK, s.stateJSON(r.Context(), s.ctl.State(), time.Now()))
}

// writeAccepted — 202 команды: фаза из Snapshot, опубликованного командой (§9.5 v2).
func (s *server) writeAccepted(w http.ResponseWriter) {
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "state": s.sched.Snapshot().Phase.String()})
}

// modelCommand — load и restart: поле model обязательно; имя попадает в журнал доступа только после успешной
// команды (его проверил автомат).
func (s *server) modelCommand(w http.ResponseWriter, r *http.Request, run func(model string) error) {
	var b struct {
		Model string `json:"model"`
	}
	if !s.adminBody(w, r, &b) {
		return
	}
	if b.Model == "" {
		writeAPIError(w, errAdminBody(`field "model" is required`))
		return
	}
	if err := run(b.Model); err != nil {
		writeAPIError(w, s.adminError(b.Model, err))
		return
	}
	noteModel(r, b.Model)
	s.writeAccepted(w)
}

func (s *server) adminLoad(w http.ResponseWriter, r *http.Request, a control.Actor) {
	s.modelCommand(w, r, func(model string) error { return s.ctl.Load(a, model) })
}

func (s *server) adminRestart(w http.ResponseWriter, r *http.Request, a control.Actor) {
	s.modelCommand(w, r, func(model string) error { return s.ctl.Restart(a, model) })
}

// adminUnload — POST /api/admin/unload: оба поля необязательны; model "" — текущая модель.
func (s *server) adminUnload(w http.ResponseWriter, r *http.Request, a control.Actor) {
	var b struct {
		Model string `json:"model"`
		Force bool   `json:"force"`
	}
	if !s.adminBody(w, r, &b) {
		return
	}
	if err := s.ctl.Unload(a, b.Model, b.Force); err != nil {
		writeAPIError(w, s.adminError(b.Model, err))
		return
	}
	if b.Model != "" {
		noteModel(r, b.Model)
	}
	s.writeAccepted(w)
}

// adminPause — POST /api/admin/pause: 200 {"paused": true, "persisted"}.
func (s *server) adminPause(w http.ResponseWriter, r *http.Request, a control.Actor) {
	var b struct {
		Force bool `json:"force"`
	}
	if !s.adminBody(w, r, &b) {
		return
	}
	persisted, err := s.ctl.Pause(a, b.Force)
	if err != nil {
		writeAPIError(w, s.adminError("", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paused": true, "persisted": persisted})
}

// adminResume — POST /api/admin/resume: 200 {"paused": false, "persisted"}.
func (s *server) adminResume(w http.ResponseWriter, r *http.Request, a control.Actor) {
	var b struct{}
	if !s.adminBody(w, r, &b) {
		return
	}
	persisted, err := s.ctl.Resume(a)
	if err != nil {
		writeAPIError(w, s.adminError("", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paused": false, "persisted": persisted})
}

// adminGetModels — GET /api/admin/models: заголовок ETag — файла на диске (в режиме legacy его нет).
func (s *server) adminGetModels(w http.ResponseWriter, r *http.Request, _ control.Actor) {
	v := s.ctl.Models(r.Context())
	if v.ETag != "" {
		w.Header().Set("ETag", v.ETag)
	}
	writeJSON(w, http.StatusOK, modelsViewJSON(v))
}

// adminPutModels — PUT /api/admin/models (§9.5 v2): шаги 1 (legacy — 409) и 2 (нет If-Match — 428) до разбора
// тела, затем тело (шаг 3) и контроллер (шаги 3–11).
func (s *server) adminPutModels(w http.ResponseWriter, r *http.Request, a control.Actor) {
	if s.live.Load().ModelsMode != config.ModeFile {
		writeAPIError(w, s.adminError("", control.ErrNotMigrated))
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writeAPIError(w, s.adminError("", control.ErrIfMatchRequired))
		return
	}
	var b struct {
		Models *[]config.DocModel `json:"models"`
	}
	if !s.adminBody(w, r, &b) {
		return
	}
	if b.Models == nil {
		writeAPIError(w, errAdminBody(`field "models" is required`))
		return
	}
	res, err := s.ctl.PutModels(a, ifMatch, &config.ModelsDoc{Models: *b.Models})
	if err != nil {
		writeAPIError(w, s.adminError("", err))
		return
	}
	writeSaved(w, res)
}

// adminReload — POST /api/admin/models/reload («Применить файл»).
func (s *server) adminReload(w http.ResponseWriter, r *http.Request, a control.Actor) {
	var b struct{}
	if !s.adminBody(w, r, &b) {
		return
	}
	res, err := s.ctl.ReloadModels(a)
	if err != nil {
		writeAPIError(w, s.adminError("", err))
		return
	}
	writeSaved(w, res)
}

// writeSaved — 200 сохранения и «Применить файл»: ETag в заголовке и в теле (§9.5 v2).
func writeSaved(w http.ResponseWriter, res control.SaveResult) {
	w.Header().Set("ETag", res.ETag)
	writeJSON(w, http.StatusOK, map[string]any{"etag": res.ETag, "restart_required": res.RestartRequired,
		"warnings": res.Warnings})
}

// adminMigrate — POST /api/admin/models/migrate (§5.8).
func (s *server) adminMigrate(w http.ResponseWriter, r *http.Request, a control.Actor) {
	var b struct{}
	if !s.adminBody(w, r, &b) {
		return
	}
	res, err := s.ctl.Migrate(a)
	if err != nil {
		writeAPIError(w, s.adminError("", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models_file": res.ModelsFile, "backup": res.Backup})
}

// adminScan — GET /api/admin/scan (§8.2, §9.5 v2).
func (s *server) adminScan(w http.ResponseWriter, r *http.Request, _ control.Actor) {
	writeJSON(w, http.StatusOK, scanJSON(s.ctl.Scan(r.Context())))
}

// adminLogs — GET /api/admin/logs: как /api/logs, имена из текущего каталога.
func (s *server) adminLogs(w http.ResponseWriter, r *http.Request, _ control.Actor) { s.apiLogs(w, r) }
