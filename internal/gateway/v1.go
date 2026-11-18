package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
)

// v1Proxy — POST /v1/{путь} (§9.3): ожидание Lease с бюджетом wait_timeout, затем прокси с контекстом запроса.
// Пока Lease нет, клиенту не отправляется ни байта.
func (s *server) v1Proxy(w http.ResponseWriter, r *http.Request) {
	model, body, ok := s.requestModel(w, r)
	if !ok {
		return
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), s.cfg.Swap.WaitTimeout) // только для ожидания (§6.9 п. 1)
	lease, err := s.sched.Acquire(waitCtx, model, false)
	cancel()
	if err != nil {
		s.reject(w, r, model, err)
		return
	}
	defer lease.Release()
	noteWait(r, lease.Waited)
	if s.afterGrant != nil {
		s.afterGrant(r)
	}
	if !lease.Valid() { // поколение Lease вышло из обслуживания (§6.9 п. 7)
		writeAPIError(w, errRetired(lease.Upstream, model))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body)) // исходные байты, тот же Content-Length (§9.3 п. 4)
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	w.Header().Set("X-Locallm-Wait-Ms", strconv.FormatInt(lease.Waited.Milliseconds(), 10))
	serveProxy(w, r, lease.Upstream)
}
