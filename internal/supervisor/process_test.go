package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/hwtest"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newTestSupervisor(t *testing.T, args ...string) (*Supervisor, config.Model) {
	t.Helper()
	exe := testutil.BuildFakeUpstream(t)
	cfg := &config.Config{Llama: config.Llama{ServerExe: exe, Port: testutil.FreePort(t),
		LoadTimeout: 30 * time.Second, StopTimeout: 2 * time.Second, LogLines: 1000}}
	s := New(cfg, nil)
	s.pollInterval = 50 * time.Millisecond
	s.portWait = 500 * time.Millisecond
	t.Cleanup(func() {
		s.terminate = func(p *os.Process) error { return p.Kill() }
		_ = s.Stop(context.Background())
	})
	return s, config.Model{ID: "a", Path: `C:\fake\model-a.gguf`, Args: args}
}

var pidRe = regexp.MustCompile(`pid=(\d+)`)

func lastPID(t *testing.T, s *Supervisor, model string) int {
	t.Helper()
	lines, _ := s.Logs(model, 1000)
	pid := 0
	for _, l := range lines {
		if m := pidRe.FindStringSubmatch(l); m != nil && strings.HasPrefix(l, "--- start") {
			pid, _ = strconv.Atoi(m[1])
		}
	}
	return pid
}

func TestStartReadyAfterLoadingAndStop(t *testing.T) {
	s, m := newTestSupervisor(t, "--loading", "2s")
	start := time.Now()
	up, err := s.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 2*time.Second {
		t.Fatalf("ready after %s, before --loading ended", el)
	}
	resp, err := http.Get(up.URL.String() + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"id":"model-a"`) {
		t.Fatalf("-m was not passed: %s", body)
	}
	st := s.Status()
	if !st.Running || st.Model != "a" || st.PID != up.PID {
		t.Fatalf("status %+v", st)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Running {
		t.Fatal("still running after Stop")
	}
	info, ok := <-up.Done
	if !ok {
		t.Fatal("Done closed without value")
	}
	if _, ok := <-up.Done; ok {
		t.Fatal("Done must be closed after one value")
	}
	_ = info
	lines, ok := s.Logs("", 1000)
	if !ok || !strings.HasPrefix(lines[0], "--- start ") {
		t.Fatalf("logs %v", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "GET /health") {
		t.Fatal("stdout health request was not captured in Logs")
	}
}

func TestStartFailLoadReturnsTail(t *testing.T) {
	s, m := newTestSupervisor(t, "--loading", "300ms", "--fail-load")
	_, err := s.Start(context.Background(), m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "exited" || le.ExitCode != 1 {
		t.Fatalf("err %#v", err)
	}
	var lf scheduler.LoadFailure
	if !errors.As(err, &lf) {
		t.Fatal("LoadError must implement scheduler.LoadFailure")
	}
	if n := strings.Count(strings.Join(le.Tail, "\n"), "simulated load failure"); n != 5 {
		t.Fatalf("tail %q", le.Tail)
	}
	if s.Status().Running {
		t.Fatal("running after failed load")
	}
}

func TestStartLoadTimeoutKillsProcess(t *testing.T) {
	s, m := newTestSupervisor(t, "--loading", "30s")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.Start(ctx, m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "timeout" {
		t.Fatalf("err %v", err)
	}
	if pid := lastPID(t, s, "a"); pid == 0 || testutil.ProcessAlive(pid) {
		t.Fatalf("process %d still alive after timeout", pid)
	}
}

func TestStartPortBusy(t *testing.T) {
	s, m := newTestSupervisor(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.cfg.Llama.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = s.Start(context.Background(), m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "port_busy" || le.ExitCode != -1 {
		t.Fatalf("err %v", err)
	}
}

func TestCrashAfterReadyDeliversExitInfo(t *testing.T) {
	s, m := newTestSupervisor(t, "--crash-after", "500ms")
	up, err := s.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-up.Done:
		if info.Code != 3 || !strings.Contains(strings.Join(info.Tail, "\n"), "simulated crash") {
			t.Fatalf("exit %+v", info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ExitInfo")
	}
	time.Sleep(50 * time.Millisecond)
	if s.Status().Running {
		t.Fatal("running after crash")
	}
}

func TestStopIdempotent(t *testing.T) {
	s, m := newTestSupervisor(t)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), m); err != nil {
		t.Fatalf("restart: %v", err)
	}
	lines, _ := s.Logs("a", 1000)
	if n := strings.Count(strings.Join(lines, "\n"), "--- start "); n != 2 {
		t.Fatalf("buffer must keep both runs, separators=%d", n)
	}
}

func TestStopUnconfirmedAfterThreeAttempts(t *testing.T) {
	s, m := newTestSupervisor(t)
	s.cfg.Llama.StopTimeout = 100 * time.Millisecond
	up, err := s.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.terminate = func(*os.Process) error { calls++; return nil }
	start := time.Now()
	if err := s.Stop(context.Background()); !errors.Is(err, scheduler.ErrStopUnconfirmed) {
		t.Fatalf("err %v", err)
	}
	if calls != 3 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("calls=%d elapsed=%s", calls, time.Since(start))
	}
	if !s.Status().Running {
		t.Fatal("Status().Running must stay true")
	}
	if _, err := s.Start(context.Background(), m); err == nil || errors.As(err, new(*LoadError)) {
		t.Fatalf("second Start must be a plain error, got %v", err)
	}
	testutil.KillPID(up.PID)
}

func TestStartTimeoutWithUnconfirmedStop(t *testing.T) {
	s, m := newTestSupervisor(t, "--loading", "30s")
	s.cfg.Llama.StopTimeout = 100 * time.Millisecond
	s.terminate = func(*os.Process) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := s.Start(ctx, m); !errors.Is(err, scheduler.ErrStopUnconfirmed) {
		t.Fatalf("err %v", err)
	}
	if !s.Status().Running {
		t.Fatal("process must stay owned")
	}
	testutil.KillPID(s.Status().PID)
}

// startJobHost запускает тестовый бинарь в режиме «хост-помощник» и возвращает PID хоста и дочернего процесса.
func startJobHost(t *testing.T, exe, model string, args []string, port int, wait time.Duration) (int, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCALLM_TEST_HELPER=jobhost", "LOCALLM_TEST_EXE="+exe, "LOCALLM_TEST_MODEL="+model,
		"LOCALLM_TEST_ARGS="+strings.Join(args, "|"), "LOCALLM_TEST_PORT="+strconv.Itoa(port))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testutil.KillPID(cmd.Process.Pid); _ = cmd.Wait() })
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "CHILD ") || strings.HasPrefix(l, "ERR") {
				lines <- l
				return
			}
		}
		close(lines)
	}()
	select {
	case l, ok := <-lines:
		if !ok || !strings.HasPrefix(l, "CHILD ") {
			t.Fatalf("job host: %q", l)
		}
		child, _ := strconv.Atoi(strings.TrimPrefix(l, "CHILD "))
		t.Cleanup(func() { testutil.KillPID(child) })
		return cmd.Process.Pid, child
	case <-time.After(wait):
		t.Fatal("job host did not report its child")
	}
	return 0, 0
}

func killHostAndWaitChild(t *testing.T, hostPID, childPID int) {
	t.Helper()
	if out, err := exec.Command("taskkill", "/f", "/pid", strconv.Itoa(hostPID)).CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v %s", err, out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for testutil.ProcessAlive(childPID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if testutil.ProcessAlive(childPID) {
		t.Fatalf("child %d survived its host", childPID)
	}
}

func TestJobObjectKillsChildWhenHostDies(t *testing.T) {
	exe := testutil.BuildFakeUpstream(t)
	host, child := startJobHost(t, exe, `C:\fake\child.gguf`, nil, testutil.FreePort(t), 30*time.Second)
	if !testutil.ProcessAlive(child) {
		t.Fatal("child not running")
	}
	killHostAndWaitChild(t, host, child)
}

func TestSpawnFailureDoesNotOwnProcess(t *testing.T) {
	s, m := newTestSupervisor(t)
	s.cfg.Llama.ServerExe = `C:\does-not-exist\llama-server.exe`
	_, err := s.Start(context.Background(), m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "spawn" || le.ExitCode != -1 || s.Status().Running {
		t.Fatalf("spawn failure: %v, status %+v", err, s.Status())
	}
}

func TestCancelledPortWaitDoesNotSpawn(t *testing.T) {
	s, m := newTestSupervisor(t)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.Llama.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Start(ctx, m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "timeout" || le.ExitCode != -1 || s.Status().Running {
		t.Fatalf("cancelled port wait: %v, status %+v", err, s.Status())
	}
}

func TestCancelledStopKeepsProcessOwned(t *testing.T) {
	s, m := newTestSupervisor(t)
	up, err := s.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	s.terminate = func(*os.Process) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := s.Stop(ctx); !errors.Is(err, scheduler.ErrStopUnconfirmed) {
		t.Fatalf("cancelled Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= s.cfg.Llama.StopTimeout {
		t.Fatalf("cancelled Stop took %s, want less than StopTimeout", elapsed)
	}
	if !s.Status().Running || !testutil.ProcessAlive(up.PID) {
		t.Fatal("cancelled Stop lost ownership of the live process")
	}
}

func TestExitTailDoesNotIncludePreviousRun(t *testing.T) {
	s, m := newTestSupervisor(t, "--fail-load", "--loading", "100ms")
	if _, err := s.Start(context.Background(), m); err == nil {
		t.Fatal("expected load failure")
	}
	m.Args = []string{"--crash-after", "300ms"}
	up, err := s.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-up.Done:
		tail := strings.Join(info.Tail, "\n")
		if strings.Contains(tail, "simulated load failure") || !strings.Contains(tail, "simulated crash") {
			t.Fatalf("tail crossed run boundary: %q", tail)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ExitInfo after crash")
	}
	if _, ok := s.Logs("unknown", 10); ok {
		t.Fatal("unknown model has logs")
	}
}

func TestArgumentValuesDoNotReachLogsOrFailure(t *testing.T) {
	const secret = "private-invalid-duration"
	s, m := newTestSupervisor(t, "--loading", secret)
	var log lockedBuffer
	s.log = slog.New(slog.NewTextHandler(&log, nil))
	_, err := s.Start(context.Background(), m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "exited" {
		t.Fatalf("expected rejected argument: %v", err)
	}
	lines, _ := s.Logs(m.ID, 1000)
	for _, out := range []string{err.Error(), strings.Join(le.Tail, "\n"), strings.Join(lines, "\n"), log.String()} {
		if strings.Contains(out, secret) {
			t.Fatal("argument value reached error or log output")
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "[redacted]") {
		t.Fatal("test did not exercise redaction")
	}
}

func TestChildEnvFiltersLlamaSettings(t *testing.T) {
	env := []string{
		"PATH=C:\\tools", "LLAMA_API_KEY=private-key", "llama_api_key=lower-key", "LlAmA_ApI_KeY=mixed-key",
		"LLAMA_ARG_HOST=0.0.0.0", "llama_arg_port=9000", "LlAmA_ArG_API_KEY=another-key", "LLAMA_ARG_=value",
		"TEMP=C:\\temp", "LLAMA_API_KEY_FILE=keep", "LLAMA_ARG=keep", "LLAMA_ARGX=keep", "MY_LLAMA_ARG_MODEL=keep=all",
	}
	want := []string{
		"PATH=C:\\tools", "TEMP=C:\\temp", "LLAMA_API_KEY_FILE=keep", "LLAMA_ARG=keep", "LLAMA_ARGX=keep", "MY_LLAMA_ARG_MODEL=keep=all",
	}
	if !reflect.DeepEqual(childEnv(env), want) {
		t.Fatal("child environment must remove llama overrides case-insensitively and preserve other entries")
	}
}

func TestModelPathStaysVisibleInLogs(t *testing.T) {
	// Positional input makes fakeupstream print the remaining argv, including -m.
	// Forward slashes keep the path literal in its quoted diagnostic.
	s, m := newTestSupervisor(t, "unexpected")
	m.Path = "C:/fake/model-visible.gguf"
	_, err := s.Start(context.Background(), m)
	var le *LoadError
	if !errors.As(err, &le) || le.Reason != "exited" || le.ExitCode != 2 {
		t.Fatalf("expected argument parsing failure: %v", err)
	}
	lines, _ := s.Logs(m.ID, 1000)
	if !strings.Contains(strings.Join(lines, "\n"), m.Path) {
		t.Fatal("model path was masked in Logs")
	}
}

func TestArgumentRedactorPreservesNormalOutputAndMasksLongSecrets(t *testing.T) {
	const normal = "n_ctx_slot = 128000\n--context-size --cache-reuse --no-webui\non off true false auto short 12345678 -12345678 0.123456789 12345678e9999"
	defaults, _ := argumentRedactor(hwtest.DefaultArgs)
	if defaults.Replace(normal) != normal {
		t.Fatal("default arguments must leave normal output byte-identical")
	}
	secrets := []string{"private-long-secret", "-private-leading-secret", "--private-double-secret"}
	redact, _ := argumentRedactor(hwtest.DefaultArgs, []string{
		"--ctx-size", "128000", "--threads", "12345678", "--seed", "-12345678", "--temperature", "0.123456789",
		"--override", "12345678e9999", "--label", "short", "--key", secrets[0], "--other-key", secrets[1], "--inline-key=" + secrets[2],
	})
	if redact.Replace(normal) != normal {
		t.Fatal("numeric, short and ordinary option values must leave normal output byte-identical")
	}
	for _, secret := range secrets {
		out := redact.Replace("credential=" + secret)
		if strings.Contains(out, secret) || !strings.Contains(out, "[redacted]") {
			t.Fatal("long credential was not masked")
		}
	}
	for _, tc := range []struct {
		name, plain, secret string
		secondSecret        string
		args                []string
	}{
		{
			name: "boolean then flag", plain: "error: invalid argument: --definitely-not-a-flag",
			args: []string{"--jinja", "--definitely-not-a-flag"},
		},
		{
			name: "boolean then flag equals value", plain: "--fail-load --loading= -key=", secret: "private-invalid-duration",
			args: []string{"--fail-load", "--loading=private-invalid-duration", "--jinja", "-key=private-invalid-duration"},
		},
		{
			name: "boolean then key and dashed value", plain: "--jinja --key", secret: "-private-leading-secret",
			args: []string{"--jinja", "--key", "-private-leading-secret"},
		},
		{
			name: "mixed case dashed value", plain: "--jinja --key", secret: "--Pr1vate_credential",
			args: []string{"--jinja", "--key", "--Pr1vate_credential"},
		},
		{
			name: "lowercase kebab value limit", plain: "--key --private-lowercase",
			args: []string{"--key", "--private-lowercase"},
		},
		{
			name: "boolean then snake_case flag and dashed value", plain: "--jinja --secret-key --ctx-size 4096", secret: "-private-leading-secret",
			secondSecret: "--secret_key",
			args:         []string{"--jinja", "--secret_key", "-private-leading-secret", "--ctx-size", "4096"},
		},
		{
			name: "lowercase snake value limit", plain: "--hf-token", secret: "--private_lowercase",
			args: []string{"--hf-token", "--private_lowercase"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redact, _ := argumentRedactor(hwtest.DefaultArgs, tc.args)
			if redact.Replace(tc.plain) != tc.plain {
				t.Fatal("flag diagnostic changed")
			}
			for _, secret := range []string{tc.secret, tc.secondSecret} {
				if secret != "" {
					out := redact.Replace("credential=" + secret)
					if strings.Contains(out, secret) || !strings.Contains(out, "[redacted]") {
						t.Fatal("long credential was not masked after a boolean flag")
					}
				}
			}
		})
	}
}

func TestReadLinesDrainsLongOutputAndRedactsBeforeTruncation(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	const secret = "sensitive-argument-value"
	redact, lookahead := argumentRedactor([]string{"--value=" + secret})
	b := newRingBuffer(10)
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(w, strings.Repeat("x", maxLineBytes-5)+secret+strings.Repeat("x", 100000)+"\r\n"+
			strings.Repeat("x", maxLineBytes-1)+"я\nlast")
		w.Close()
		written <- err
	}()
	readLines(r, b, redact, lookahead)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	lines := b.Last(10)
	if len(lines) != 3 || lines[2] != "last" || strings.Contains(lines[0], secret[:5]) {
		t.Fatalf("long output was not safely drained: lengths %d", len(lines))
	}
	for _, line := range lines {
		if len(line) > maxLineBytes || !utf8.ValidString(line) {
			t.Fatal("line exceeds the byte limit or has broken UTF-8")
		}
	}
}

func TestHealthContinuesOnUnexpectedStatusAndLogsOnce(t *testing.T) {
	var log bytes.Buffer
	s := New(&config.Config{}, slog.New(slog.NewTextHandler(&log, nil)))
	s.pollInterval = time.Millisecond
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
		} else if n == 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.waitReady(ctx, &proc{exited: make(chan struct{})}, u); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || strings.Count(log.String(), "unexpected /health answer") != 1 {
		t.Fatalf("health calls=%d log=%s", calls.Load(), log.String())
	}
}
