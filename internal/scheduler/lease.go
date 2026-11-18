package scheduler

import (
	"context"
	"sync"
	"time"
)

// Lease — право одного запроса на поколение Gen (§6.9).
type Lease struct {
	Model    string
	Gen      uint64
	Upstream *Upstream
	Waited   time.Duration

	once    sync.Once
	release func()
}

var cancelledCtx = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// Valid — false, если поколение Lease выведено из обслуживания (Upstream.Retire, §6.9 п. 6).
func (l *Lease) Valid() bool { return l.Upstream != nil && !l.Upstream.Retired() }

// Context отменяется при Retire поколения Lease (§6.9 п. 7).
func (l *Lease) Context() context.Context {
	if l.Upstream == nil {
		return cancelledCtx
	}
	return l.Upstream.Context()
}

// Release отправляет событие Release{Gen} ровно один раз; шлюз вызывает в defer.
func (l *Lease) Release() {
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}
