package scheduler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

var errUpstreamRetired = errors.New("upstream retired")

// RetireReason — причина снятия поколения (§6.2, §7.4 спецификации v2); 0 — поколение не снято.
type RetireReason int32

const (
	RetireStop   RetireReason = iota + 1 // P8: обычная остановка после Draining
	RetireExited                         // E9: процесс завершился сам
	RetireForced                         // F1: принудительная выгрузка администратором
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Upstream — один запуск llama-server (§4.4). Transport свой на каждый запуск; после Retire
// он не открывает новых соединений, а соединение, установленное во время Retire, закрывается.
type Upstream struct {
	Model     string
	URL       *url.URL
	PID       int
	Transport *http.Transport
	Done      <-chan ExitInfo

	ctx    context.Context
	cancel context.CancelFunc
	reason atomic.Int32 // RetireReason первого Retire; 0 — не снят
}

func NewUpstream(model string, u *url.URL, pid int, done <-chan ExitInfo) *Upstream {
	d := &net.Dialer{Timeout: 2 * time.Second}
	return newUpstream(model, u, pid, done, d.DialContext)
}

func newUpstream(model string, u *url.URL, pid int, done <-chan ExitInfo, dial dialFunc) *Upstream {
	ctx, cancel := context.WithCancel(context.Background())
	up := &Upstream{Model: model, URL: u, PID: pid, Done: done, ctx: ctx, cancel: cancel}
	up.Transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if up.Retired() {
				return nil, errUpstreamRetired
			}
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if up.Retired() { // Retire во время dial (§6.9 п. 8)
				c.Close()
				return nil, errUpstreamRetired
			}
			return c, nil
		},
		DisableCompression:    true,
		ResponseHeaderTimeout: 0,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
	return up
}

// Context отменяется Retire.
func (u *Upstream) Context() context.Context { return u.ctx }

// Retired — true после Retire.
func (u *Upstream) Retired() bool { return u.reason.Load() != 0 }

// RetireReason — причина первого Retire; 0 — поколение не снято. Шлюз выбирает по ней текст 502 (§7.4 v2).
func (u *Upstream) RetireReason() RetireReason { return RetireReason(u.reason.Load()) }

// Retire — идемпотентно, сохраняется причина первого вызова (r == 0 — RetireStop); вызывает только владелец
// планировщика. Причина записывается раньше отмены Context: запрос, увидевший отмену, видит и причину. Не ждёт I/O.
func (u *Upstream) Retire(r RetireReason) {
	if r == 0 {
		r = RetireStop
	}
	if !u.reason.CompareAndSwap(0, int32(r)) {
		return
	}
	u.cancel()
	u.Transport.CloseIdleConnections()
}
