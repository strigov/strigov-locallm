package control

import (
	"context"
	"errors"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Shutdown — шаги 2–4 штатного выхода (§11.6). Срок ctx ограничивает весь вызов целиком, а не только ожидание фазы:
// последовательность (мьютекс паузы — навсегда; sched.Pause(true); closeServer(); цикл ожидания фазы со своим
// case <-ctx.Done()) выполняет рабочая горутина и отправляет результат в канал с буфером 1; Shutdown берёт результат
// из канала либо ошибку срока — что наступит раньше. По сроку рабочая горутина может ещё завершаться в фоне. Берёт
// мьютекс паузы и не отпускает его до выхода процесса: «Продолжить» из трея или /api/admin/resume не снимет паузу во
// время выхода. Scheduler.Pause(true) — только в памяти, state.json не пишется. closeServer (http.Server.Close) —
// после паузы. Halted важнее срока: какой бы путь ни дал ошибку срока (из канала или из внешней ветки), по
// Halted в свежем снимке возвращается scheduler.ErrHalted — побеждает путь onFatal (код 7), а не штатный код 0.
// Ошибка Pause возвращается как есть (сервер всё равно закрыт). Вызывается один раз за жизнь процесса.
func (c *Controller) Shutdown(ctx context.Context, closeServer func()) error {
	done := make(chan error, 1)
	go func() {
		done <- c.shutdown(ctx, closeServer)
	}()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && c.sched.Snapshot().Halted {
		return scheduler.ErrHalted // Halted важнее срока: побеждает путь onFatal (код 7), а не код 0
	}
	return err
}

// shutdown — последовательность выхода в рабочей горутине. Мьютекс паузы берётся первым и не отпускается: процесс
// выходит, снимать паузу некому.
func (c *Controller) shutdown(ctx context.Context, closeServer func()) error {
	c.pauseMu.Lock() // не отпускается: процесс выходит
	err := c.sched.Pause(true)
	closeServer()
	if err != nil {
		return err
	}
	t := time.NewTicker(c.shutdownPoll)
	defer t.Stop()
	for {
		s := c.sched.Snapshot()
		switch {
		case s.Halted:
			return scheduler.ErrHalted
		case s.Phase == scheduler.PhaseEmpty || s.Phase == scheduler.PhaseWaitVRAM || s.Phase == scheduler.PhaseLoading:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
