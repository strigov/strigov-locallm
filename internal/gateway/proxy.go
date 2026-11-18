package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"

	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// serveProxy — §9.3 п. 5–7. Путь, query и тело — без изменений; Authorization удаляется (llama-server
// слушает loopback без ключа); каждая запись сбрасывается клиенту; исходящий запрос отменяется и при
// уходе клиента, и при Retire. ModifyResponse нет: ответы llama-server, включая ошибки, проходят как есть.
func serveProxy(w http.ResponseWriter, r *http.Request, up *scheduler.Upstream) {
	if up.Retired() {
		writeAPIError(w, errRetired(up, up.Model))
		return
	}
	clientCtx := r.Context()
	ctx, cancel := context.WithCancel(clientCtx)
	defer cancel()
	stop := context.AfterFunc(up.Context(), cancel)
	defer stop()
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.URL)
			pr.Out.Header.Del("Authorization")
		},
		Transport:     up.Transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			switch {
			case clientCtx.Err() != nil: // клиент ушёл — ничего не писать
			case up.Retired():
				writeAPIError(w, errRetired(up, up.Model))
			default:
				writeAPIError(w, errUpstreamUnreachable(up.Model, err))
			}
		},
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}
