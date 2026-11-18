// Package scheduler — единственный владелец решения «какая модель в VRAM» (§6).
// План 1 создаёт здесь контракты с супервизором; автомат, цикл и Lease добавляет план 2.
package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// Backend объявлен потребителем; реализует internal/supervisor (§4.4).
type Backend interface {
	// Start запускает llama-server для m и возвращается, когда GET /health == 200, процесс
	// завершился или истёк ctx (load_timeout). LoadFailure — процесс завершён и дождан (или не стартовал).
	// ErrStopUnconfirmed — по истечении load_timeout процесс не удалось завершить. Start при живом процессе — ошибка.
	Start(ctx context.Context, m config.Model) (*Upstream, error)
	// Stop: TerminateProcess + ожидание до stop_timeout, до 3 попыток. nil — только при подтверждённом
	// выходе (или если процесса нет); идемпотентен. Иначе ErrStopUnconfirmed, Status().Running == true.
	// Отмена ctx до подтверждения выхода — тоже ErrStopUnconfirmed; вызывающий, которому нужны
	// все 3 попытки, передаёт context.Background() (так делает планировщик).
	Stop(ctx context.Context) error
	// Status — мгновенный снимок процесса.
	Status() BackendStatus
}

var ErrStopUnconfirmed = errors.New("llama-server did not exit after TerminateProcess")

type ExitInfo struct {
	Code int
	At   time.Time
	Tail []string
}

type BackendStatus struct {
	Running bool
	Model   string
	PID     int
	Since   time.Time
}

// LoadFailure — неудачная загрузка с подтверждённым выходом процесса (поправка к §4.4:
// *supervisor.LoadError реализует этот интерфейс, чтобы scheduler не импортировал supervisor).
type LoadFailure interface {
	error
	LoadFailure() (reason string, exitCode int, tail []string)
}
