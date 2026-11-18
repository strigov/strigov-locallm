package scheduler

// SetBeforeWaiterGone — тестовый шов: f вызывается в Acquire после отмены ctx и до отправки WaiterGone.
// Вызывать до Run.
func SetBeforeWaiterGone(s *Scheduler, f func()) { s.beforeGone = f }
