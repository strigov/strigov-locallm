// Package gateway — HTTP-шлюз хоста (§9). План 1 создаёт формат ошибок и прокси; маршруты добавляет план 2.
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// apiError — ошибка в формате OpenAI (§9.6).
type apiError struct {
	Status  int
	Code    string
	Type    string
	Message string
	Extra   map[string]any    // доп. поля внутри "error"
	Headers map[string]string // Retry-After, x-should-retry, WWW-Authenticate, Allow
}

// writeAPIError пишет {"error":{"message","type","code",...Extra}}; Extra не перекрывает три основных поля.
func writeAPIError(w http.ResponseWriter, e *apiError) {
	body := make(map[string]any, len(e.Extra)+3)
	for k, v := range e.Extra {
		body[k] = v
	}
	body["message"], body["type"], body["code"] = e.Message, e.Type, e.Code
	h := w.Header()
	for k, v := range e.Headers {
		h.Set(k, v)
	}
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	w.WriteHeader(e.Status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"error": body})
}

// errUpstreamUnreachable — 502: llama-server недоступен до отправки заголовков ответа.
func errUpstreamUnreachable(model string, err error) *apiError {
	return &apiError{Status: http.StatusBadGateway, Code: "upstream_error", Type: "server_error",
		Message: fmt.Sprintf("llama-server for %q is unreachable: %v", model, err)}
}

// errUpstreamExited — 502: процесс поколения Lease вышел (§6.9 п. 7).
func errUpstreamExited(model string) *apiError {
	return &apiError{Status: http.StatusBadGateway, Code: "upstream_error", Type: "server_error",
		Message: fmt.Sprintf("llama-server for %q exited", model)}
}

// errUpstreamAborted — 502: поколение Lease снято принудительной выгрузкой (§6.7.2, §9.6 v2).
func errUpstreamAborted(model string) *apiError {
	return &apiError{Status: http.StatusBadGateway, Code: "upstream_error", Type: "server_error",
		Message: fmt.Sprintf("request aborted: model %q was unloaded by the administrator", model)}
}

// errRetired — 502 для снятого поколения: текст по причине первого Retire (§7.4 v2).
func errRetired(up *scheduler.Upstream, model string) *apiError {
	if up != nil && up.RetireReason() == scheduler.RetireForced {
		return errUpstreamAborted(model)
	}
	return errUpstreamExited(model)
}
