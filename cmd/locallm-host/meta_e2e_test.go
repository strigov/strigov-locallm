package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

// loadedMeta — объект meta модели id в ответе GET path (nil — элемента meta нет).
func (e *e2eHost) loadedMeta(t *testing.T, path, id string) map[string]any {
	t.Helper()
	resp, body, err := e.call(http.MethodGet, path, "", e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %v %v %s", path, err, resp, body)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("%s: %v %s", path, err, body)
	}
	for _, it := range list.Data {
		if it["id"] == id {
			meta, _ := it["meta"].(map[string]any)
			return meta
		}
	}
	t.Fatalf("%s: no model %q", path, id)
	return nil
}

// Сквозной тест §8.3: настоящий exe отдаёт meta.n_ctx_train и meta.n_embd загруженной модели из заголовка её файла.
func TestE2EModelsMetaFromHeader(t *testing.T) {
	e := startServingWith(t, testConf{DefaultModel: "a", Headers: true})
	e.waitReady(t, "a")
	schedtest.Eventually(t, 10*time.Second, func() bool {
		for _, path := range []string{"/models", "/v1/models"} {
			meta := e.loadedMeta(t, path, "a")
			if meta["n_ctx_train"] != float64(131072) || meta["n_embd"] != float64(4096) {
				return false
			}
		}
		return true
	})
}
