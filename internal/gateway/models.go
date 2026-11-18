package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// modelsBody — тело GET /models (v1 == false, §9.4) или GET /v1/models (v1 == true, §9.3): модели в порядке
// каталога автомата (Snapshot.Catalog, §6.8 v2), статусы по §6.8, поправки формы из shape.go. Путь, контекст и
// input_modalities — из eff(m): пока модель не перезапущена, клиент видит значения работающего процесса.
func (s *server) modelsBody(v1 bool, snap *scheduler.Snapshot, now time.Time) map[string]any {
	shape := modelsShape
	if v1 {
		shape = v1ModelsShape
	}
	data := make([]any, 0, len(snap.Catalog))
	for _, d := range snap.Catalog {
		m := eff(snap, d)
		st := snap.ModelStatus(d.ID, now)
		// status.args есть у всех элементов эталонов (набор полей §9.4), но argv клиенту не отдаётся: всегда пусто (F2-D6).
		status := map[string]any{"value": st, "args": []string{}}
		if st == scheduler.StatusFailed {
			status["value"], status["failed"], status["exit_code"] = failedValue, true, snap.Failed[d.ID].ExitCode
		}
		var item map[string]any
		if v1 {
			item = map[string]any{"id": d.ID, "object": "model", "created": s.started.Unix(), "owned_by": "locallm", "status": status}
			if m.ContextLength > 0 {
				item["context_length"] = m.ContextLength
			}
		} else {
			item = map[string]any{"id": d.ID, "path": m.Path, "status": status}
		}
		applyShape(item, shape[st])
		// F4-PM1, F4-PM2 (06, «Поправки к спецификации»): настоящие meta.n_ctx и input_modalities — и в /models, и в /v1/models.
		// n_ctx_train и n_embd — из заголовка файла eff(m) в кэше GGUF (§8.3), без ожидания диска.
		if meta, ok := item["meta"].(map[string]any); ok {
			meta["n_ctx"] = m.ContextLength
			meta["n_ctx_train"], meta["n_embd"] = s.headerMeta(m.Path)
		}
		if architecture, ok := item["architecture"].(map[string]any); ok {
			architecture["input_modalities"] = s.modalities(m)
		}
		data = append(data, item)
	}
	body := map[string]any{"data": data}
	if v1 {
		body["object"] = "list"
	}
	applyShape(body, shape["_top"])
	return body
}

// eff — определение, с которым работает процесс модели (§6.8 v2): Running, если он запущен для def.ID, иначе def.
// Running очищается при остановке процесса.
func eff(snap *scheduler.Snapshot, def config.Model) config.Model {
	if snap.Running.ID != "" && snap.Running.ID == def.ID {
		return snap.Running
	}
	return def
}

// modalities — input_modalities модели: "image", если итоговая командная строка содержит --mmproj или -mm (F4-PM2).
// Аргументы берутся из конфига старта: глобальные llama.* меняются только перезапуском хоста.
func (s *server) modalities(m config.Model) []string {
	if hasImageInput(s.cfg.Argv(m)[1:]) {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

// A matching token used as another flag's value is an accepted false positive.
func hasImageInput(args []string) bool {
	for _, arg := range args {
		if arg == "--mmproj" || arg == "-mm" || strings.HasPrefix(arg, "--mmproj=") {
			return true
		}
	}
	return false
}

func (s *server) v1Models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.modelsBody(true, s.sched.Snapshot(), time.Now()))
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.modelsBody(false, s.sched.Snapshot(), time.Now()))
}

// modelsLoad — POST /models/load: явное намерение, ответ сразу после публикации Snapshot (§9.4).
// 409 и 429 не возвращает никогда.
func (s *server) modelsLoad(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.requestModel(w, r)
	if !ok {
		return
	}
	if err := s.sched.Load(model); err != nil {
		writeAPIError(w, s.cmdError(model, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// modelsUnload — POST /models/unload: выгрузка незагруженной модели — тоже успех (снимает метку failed).
func (s *server) modelsUnload(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.requestModel(w, r)
	if !ok {
		return
	}
	if err := s.sched.Unload(model); err != nil {
		writeAPIError(w, s.cmdError(model, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// props — GET /props (F5-D1): ответ llama-server текущей модели как есть, строка запроса не меняется. Только под Lease
// и только на загруженную модель: AcquireLoaded не ставит ожидающего, поэтому autoload клиента ничего не значит, а
// загрузка и пересадка не начинаются. Модель не загружена — 502 upstream_error, модели нет в конфиге — 404.
// Тело проходит проверки §9.1, как в apiUnload, но не пересылается.
func (s *server) props(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readBody(w, r); !ok {
		return // ответ (400 или 413) уже записан
	}
	model := r.URL.Query().Get("model")
	if cfg := s.live.Load(); model != "" {
		if _, known := cfg.Model(model); !known {
			writeAPIError(w, errModelNotFound(model, cfg.ModelIDs()))
			return
		}
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), s.cfg.Swap.WaitTimeout) // ответ приходит сразу; ctx — на случай Halted (E15)
	lease, err := s.sched.AcquireLoaded(waitCtx, model)
	cancel()
	if errors.Is(err, scheduler.ErrModelNotLoaded) {
		writeAPIError(w, errUpstreamUnreachable(model, err))
		return
	}
	if err != nil {
		s.reject(w, r, model, err)
		return
	}
	defer lease.Release()
	noteModel(r, lease.Model)
	if s.afterGrant != nil {
		s.afterGrant(r)
	}
	if !lease.Valid() { // поколение Lease вышло из обслуживания (§6.9 п. 7)
		writeAPIError(w, errRetired(lease.Upstream, lease.Model))
		return
	}
	r.Body, r.ContentLength, r.TransferEncoding = http.NoBody, 0, nil
	serveProxy(w, r, lease.Upstream)
}
