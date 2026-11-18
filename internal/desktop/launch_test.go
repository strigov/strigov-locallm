package desktop

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
	"golang.org/x/sys/windows"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func debugLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type started struct {
	exe   string
	args  []string
	flags uint32
}

// fakeStart подменяет startProcess и системные каталоги; errs — ответы по порядку вызовов (nil — успех, pid 4242).
func fakeStart(t *testing.T, errs ...error) *[]started {
	t.Helper()
	calls := &[]started{}
	prevStart, prevSys, prevWin := startProcess, systemDir, windowsDir
	t.Cleanup(func() { startProcess, systemDir, windowsDir = prevStart, prevSys, prevWin })
	systemDir = func() (string, error) { return `C:\Windows\System32`, nil }
	windowsDir = func() (string, error) { return `C:\Windows`, nil }
	startProcess = func(exe string, args []string, flags uint32) (int, error) {
		*calls = append(*calls, started{exe, slices.Clone(args), flags})
		if n := len(*calls); n <= len(errs) && errs[n-1] != nil {
			return 0, errs[n-1]
		}
		return 4242, nil
	}
	return calls
}

// §11.3: полный путь к exe из системного каталога, CREATE_BREAKAWAY_FROM_JOB, без cmd /c start.
func TestOpenCommands(t *testing.T) {
	calls := fakeStart(t)
	const addr = "http://127.0.0.1:8080/ui/#otp=abc_-DEF"
	if err := OpenURL(discard(), addr); err != nil {
		t.Fatal(err)
	}
	if err := OpenFile(discard(), `C:\Users\me\AppData\Local\locallm\logs\host.log`); err != nil {
		t.Fatal(err)
	}
	if err := OpenFolder(discard(), `C:\Users\me\AppData\Local\locallm`); err != nil {
		t.Fatal(err)
	}
	want := []started{
		{`C:\Windows\System32\rundll32.exe`, []string{"url.dll,FileProtocolHandler", addr}, windows.CREATE_BREAKAWAY_FROM_JOB},
		{`C:\Windows\System32\notepad.exe`, []string{`C:\Users\me\AppData\Local\locallm\logs\host.log`}, windows.CREATE_BREAKAWAY_FROM_JOB},
		{`C:\Windows\explorer.exe`, []string{`C:\Users\me\AppData\Local\locallm`}, windows.CREATE_BREAKAWAY_FROM_JOB},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("started:\n%+v\nwant:\n%+v", *calls, want)
	}
}

// §7.1, §11.3: внешний job не разрешает выход — повтор без флага и предупреждение в журнале.
func TestLaunchRetriesWithoutBreakaway(t *testing.T) {
	calls := fakeStart(t, windows.ERROR_ACCESS_DENIED)
	var buf bytes.Buffer
	pid, breakaway, err := launch(debugLog(&buf), "browser", `C:\Windows\System32\rundll32.exe`, "x")
	if err != nil || breakaway || pid != 4242 {
		t.Fatalf("pid %d, breakaway %v, err %v", pid, breakaway, err)
	}
	if len(*calls) != 2 || (*calls)[0].flags != windows.CREATE_BREAKAWAY_FROM_JOB || (*calls)[1].flags != 0 {
		t.Fatalf("calls: %+v", *calls)
	}
	log := buf.String()
	if !strings.Contains(log, `level=WARN msg="launch outside the job was refused; starting inside it" what=browser breakaway=false`) ||
		!strings.Contains(log, `level=DEBUG msg=launched what=browser breakaway=false`) {
		t.Fatalf("log:\n%s", log)
	}
	buf.Reset()
	if _, breakaway, err := launch(debugLog(&buf), "notepad", `C:\Windows\System32\notepad.exe`); err != nil || !breakaway ||
		!strings.Contains(buf.String(), `msg=launched what=notepad breakaway=true`) {
		t.Fatalf("breakaway %v, err %v, log:\n%s", breakaway, err, buf.String())
	}
}

// Другая ошибка запуска не повторяется; текст — "start <what>: <ошибка>".
func TestLaunchOtherErrorDoesNotRetry(t *testing.T) {
	calls := fakeStart(t, errors.New("boom"))
	if _, _, err := launch(discard(), "notepad", `C:\Windows\System32\notepad.exe`, `C:\x.log`); err == nil ||
		err.Error() != "start notepad: boom" || len(*calls) != 1 {
		t.Fatalf("err %v, calls %+v", err, *calls)
	}
}

// §11.3: адреса собирает только хост — схема http, есть хост, нет пробелов, кавычек и управляющих символов;
// пути — абсолютные. Отказ — без запуска.
func TestOpenRefusesForeignInput(t *testing.T) {
	calls := fakeStart(t)
	for _, u := range []string{
		"",
		"https://127.0.0.1:8080/ui/",
		"file:///C:/Windows/win.ini",
		"javascript:alert(1)",
		"http:///ui/",
		"http://127.0.0.1:8080/ui/ #otp=x",
		`http://127.0.0.1:8080/ui/"&calc`,
		"http://127.0.0.1:8080/ui/\n",
		"http://127.0.0.1:8080/ui/\t#otp=x",
		"http://127.0.0.1:8080/ui/\u0085",
		"http://127.0.0.1:8080/ui/\u00a0",
	} {
		if err := OpenURL(discard(), u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if err := OpenFile(discard(), `logs\host.log`); err == nil {
		t.Error("relative file path accepted")
	}
	if err := OpenFolder(discard(), `locallm`); err == nil {
		t.Error("relative folder path accepted")
	}
	if len(*calls) != 0 {
		t.Fatalf("started: %+v", *calls)
	}
}

// §13: одноразовый код из адреса не попадает ни в текст ошибки, ни в журнал.
func TestOpenURLErrorHidesAddress(t *testing.T) {
	fakeStart(t, errors.New(`exec: "C:\\Windows\\System32\\rundll32.exe": file does not exist`))
	var buf bytes.Buffer
	err := OpenURL(debugLog(&buf), "http://127.0.0.1:8080/ui/#otp=SECRETCODE")
	if err == nil || strings.Contains(err.Error(), "SECRETCODE") || strings.Contains(buf.String(), "SECRETCODE") {
		t.Fatalf("err %v, log:\n%s", err, buf.String())
	}
}

// §7.1, F1-D1 (ledger первого плана): процесс, запущенный с CREATE_BREAKAWAY_FROM_JOB, переживает закрытие job хоста,
// без флага — нет. Если тест сам работает в job, который запрещает выход (повтор без флага), проверять нечего.
func TestBreakawayChildOutlivesJob(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCALLM_TEST_HELPER=launcher")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "PIDS ") || strings.HasPrefix(l, "ERR") {
				lines <- l
				return
			}
		}
		close(lines)
	}()
	var outside, inside int
	var breakaway bool
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("launcher exited without output")
		}
		if _, err := fmt.Sscanf(l, "PIDS %d %d %t", &outside, &inside, &breakaway); err != nil {
			t.Fatalf("launcher: %q", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("launcher did not report")
	}
	t.Cleanup(func() { testutil.KillPID(outside); testutil.KillPID(inside) })
	if !breakaway {
		t.Skip("the test runs inside a job that forbids breakaway (ERROR_ACCESS_DENIED): launch fell back to the job")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	schedtest.Eventually(t, 5*time.Second, func() bool { return !testutil.ProcessAlive(inside) })
	if !testutil.ProcessAlive(outside) {
		t.Fatal("the process started with CREATE_BREAKAWAY_FROM_JOB died with the job")
	}
}
