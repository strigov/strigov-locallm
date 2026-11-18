package desktop

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/supervisor"
)

// Процессы-помощники TestBreakawayChildOutlivesJob. launcher входит в job хоста (supervisor.EnterJob) и запускает два
// процесса sleep: через launch (с выходом из job) и через startProcess с флагами 0 (остаётся в job).
func TestMain(m *testing.M) {
	switch os.Getenv("LOCALLM_TEST_HELPER") {
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "launcher":
		os.Exit(launcher())
	}
	os.Exit(m.Run())
}

func launcher() int {
	if err := supervisor.EnterJob(); err != nil {
		fmt.Println("ERR", err)
		return 3
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Println("ERR", err)
		return 4
	}
	_ = os.Setenv("LOCALLM_TEST_HELPER", "sleep") // дети — тот же тестовый бинарь в режиме sleep
	outside, breakaway, err := launch(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", self)
	if err != nil {
		fmt.Println("ERR", err)
		return 5
	}
	inside, err := startProcess(self, nil, 0)
	if err != nil {
		fmt.Println("ERR", err)
		return 6
	}
	fmt.Printf("PIDS %d %d %t\n", outside, inside, breakaway)
	// Тест убивает процесс раньше. time.Sleep, а не select{}: без других горутин select{} — фатальный deadlock.
	time.Sleep(2 * time.Minute)
	return 0
}
