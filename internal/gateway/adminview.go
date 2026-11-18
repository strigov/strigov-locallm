package gateway

import (
	"context"
	"time"

	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/gguf"
)

// stateJSON — тело GET /api/admin/state (§9.5 v2): поля /api/status, pause_saved, restart_required, config и models.
func (s *server) stateJSON(ctx context.Context, v control.StateView, now time.Time) map[string]any {
	body := s.statusBody(ctx, v.Snap, now)
	body["pause_saved"] = v.PauseSaved
	body["restart_required"] = v.Snap.RestartRequired()
	body["config"] = map[string]any{"mode": v.Cfg.ModelsMode, "models_file": v.Cfg.ModelsFile,
		"applied_etag": nullString(v.AppliedETag), "disk_etag": nullString(v.DiskETag), "out_of_sync": v.OutOfSync}
	body["models"] = s.stateModels(v, now)
	return body
}

// stateModels — модели каталога автомата: статус §6.8, context_length из eff(m); next — значения def(m) после
// перезапуска, если работающий процесс запущен не с ними (Running.ID == m и !Running.SameLaunch(def(m))).
func (s *server) stateModels(v control.StateView, now time.Time) []any {
	sn := v.Snap
	out := make([]any, 0, len(sn.Catalog))
	for _, d := range sn.Catalog {
		var failed, next any
		if f, ok := sn.Failed[d.ID]; ok && f.Active(now) {
			failed = map[string]any{"exit_code": f.ExitCode, "message": f.Message, "retry_in_ms": f.Until.Sub(now).Milliseconds()}
		}
		if sn.Running.ID == d.ID && !sn.Running.SameLaunch(d) {
			next = map[string]any{"context_length": d.ContextLength, "input_modalities": s.modalities(d)}
		}
		out = append(out, map[string]any{"id": d.ID, "base": d.Base, "profile": d.Profile,
			"status": sn.ModelStatus(d.ID, now), "context_length": eff(sn, d).ContextLength,
			"default": d.ID == v.Cfg.Swap.DefaultModel, "failed": failed, "next": next})
	}
	return out
}

// modelsViewJSON — тело GET /api/admin/models (§9.5 v2). Срезы вида не nil: в JSON — [], а не null.
func modelsViewJSON(v control.ModelsView) map[string]any {
	models := make([]any, 0, len(v.Models))
	for _, m := range v.Models {
		profiles := make([]any, 0, len(m.Profiles))
		for _, p := range m.Profiles {
			profiles = append(profiles, map[string]any{"name": p.Name, "args": p.Args, "hand_only": p.HandOnly})
		}
		var info any
		if m.Info != nil {
			info = infoJSON(m.Info)
		}
		models = append(models, map[string]any{"id": m.ID, "path": m.Path, "path_hand_only": m.PathHandOnly,
			"args": m.Args, "args_hand_only": m.ArgsHandOnly, "profiles": profiles,
			"hand_only_reasons": m.HandOnlyReasons, "info": info, "warnings": m.Warnings})
	}
	return map[string]any{"mode": v.Mode, "models_file": v.ModelsFile, "models_dirs": v.ModelsDirs,
		"default_args": v.DefaultArgs, "default_model": v.DefaultModel, "out_of_sync": v.OutOfSync,
		"disk_errors": v.DiskErrors, "models": models}
}

// infoJSON — заголовок GGUF модели (§8.3) для GET /api/admin/models.
func infoJSON(i *gguf.Info) map[string]any {
	return map[string]any{"kind": i.Kind, "architecture": i.Architecture, "name": i.Name,
		"context_train": i.ContextTrain, "split_count": i.SplitCount, "size_bytes": i.Size, "partial": i.Partial}
}

// scanJSON — тело GET /api/admin/scan (§9.5 v2). У файла error всегда "": ошибки чтения приходят в errors.
func scanJSON(v control.ScanView) map[string]any {
	files := make([]any, 0, len(v.Files))
	for _, f := range v.Files {
		mmproj := f.MMProj
		if mmproj == nil {
			mmproj = []string{}
		}
		files = append(files, map[string]any{"path": f.Path, "size_bytes": f.Info.Size, "kind": f.Info.Kind,
			"architecture": f.Info.Architecture, "name": f.Info.Name, "context_train": f.Info.ContextTrain,
			"split_count": f.Info.SplitCount, "shards_ok": f.ShardsOK, "mmproj": mmproj, "used_by": f.UsedBy,
			"error": ""})
	}
	errs := make([]any, 0, len(v.Errors))
	for _, e := range v.Errors {
		errs = append(errs, map[string]any{"path": e.Path, "message": e.Message})
	}
	return map[string]any{"roots": v.Roots, "truncated": v.Truncated, "elapsed_ms": v.Elapsed.Milliseconds(),
		"files": files, "errors": errs}
}
