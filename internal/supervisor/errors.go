package supervisor

import "fmt"

// LoadError — загрузка не удалась; процесс уже завершён и дождан или не стартовал (§4.4).
type LoadError struct {
	Reason   string   // "exited" | "timeout" | "port_busy" | "spawn"
	ExitCode int      // -1, если процесс не стартовал
	Tail     []string // хвост вывода текущего запуска (§7.5)
}

func (e *LoadError) Error() string {
	switch e.Reason {
	case "exited":
		return fmt.Sprintf("llama-server exited during load with code %d", e.ExitCode)
	case "timeout":
		return "llama-server was not ready within load_timeout"
	case "port_busy":
		return "llama-server port is busy"
	case "spawn":
		return "cannot start llama-server"
	}
	return "llama-server load failed: " + e.Reason
}

// LoadFailure реализует scheduler.LoadFailure.
func (e *LoadError) LoadFailure() (string, int, []string) { return e.Reason, e.ExitCode, e.Tail }
