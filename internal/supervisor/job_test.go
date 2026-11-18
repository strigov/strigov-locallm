package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// §7.1 v2: job хоста — ровно KILL_ON_JOB_CLOSE | BREAKAWAY_OK; SILENT_BREAKAWAY_OK не ставится никогда. Флаги
// читаются у настоящего job процесса-помощника: сам тестовый процесс в job не входит.
func TestEnterJobFlags(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCALLM_TEST_HELPER=jobflags")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	want := fmt.Sprintf("FLAGS %#x", uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE|windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK))
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("job flags: got %q, want %q", got, want)
	}
}
