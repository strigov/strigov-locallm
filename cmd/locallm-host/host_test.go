// Сквозные тесты exe (план 2 Ф5): locallm-host.exe собирается с тегом e2efault (консольная сборка)
// и запускается отдельным процессом с --no-dialog --log-stderr; вместо llama-server — fakeupstream.exe.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

func TestMain(m *testing.M) {
	code := testutil.Main(m) // удаляет и сборку fakeupstream
	if hostDir != "" {
		_ = os.RemoveAll(hostDir)
	}
	os.Exit(code)
}

var (
	hostOnce sync.Once
	hostDir  string
	hostExe  string
	hostErr  error
)

// buildHost собирает консольный locallm-host.exe с тегом e2efault один раз на тестовый бинарь.
// Рабочий каталог go test — каталог пакета, поэтому собирается ".".
func buildHost(t *testing.T) string {
	t.Helper()
	hostOnce.Do(func() {
		if hostDir, hostErr = os.MkdirTemp("", "locallm-host-e2e-"); hostErr != nil {
			return
		}
		hostExe = filepath.Join(hostDir, "locallm-host.exe")
		cmd := exec.Command("go", "build", "-tags", "e2efault", "-ldflags", "-X main.version=0.1.0-e2e", "-o", hostExe, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			hostErr = fmt.Errorf("go build -tags e2efault: %v\n%s", err, b)
		}
	})
	if hostErr != nil {
		t.Fatal(hostErr)
	}
	return hostExe
}

// lockedBuffer — буфер, в который пишут несколько горутин (вывод процесса, журнал hw-теста).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withEnv заменяет переменные окружения; имя сравнивается без учёта регистра (в Windows Path и PATH — одна переменная).
func withEnv(base []string, kv ...string) []string {
	out := make([]string, 0, len(base)+len(kv))
	for _, e := range base {
		name, _, _ := strings.Cut(e, "=")
		keep := true
		for _, s := range kv {
			if n, _, _ := strings.Cut(s, "="); strings.EqualFold(name, n) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return append(out, kv...)
}

type hostProc struct {
	cmd     *exec.Cmd
	out     *lockedBuffer  // stdout и stderr хоста; с --log-stderr — и копия журнала
	stdin   io.WriteCloser // для LOCALLM_E2E_QUIT=stdin
	appData string         // LOCALAPPDATA процесса: туда пишется журнал до открытия основного (FallbackPath)
	done    chan struct{}
	code    int
}

// startHost запускает собранный хост с --no-dialog --log-stderr --no-tray и временным LOCALAPPDATA.
func startHost(t *testing.T, args []string, env ...string) *hostProc {
	t.Helper()
	p := &hostProc{out: &lockedBuffer{}, appData: t.TempDir(), done: make(chan struct{})}
	p.cmd = exec.Command(buildHost(t), append([]string{"--no-dialog", "--log-stderr", "--no-tray"}, args...)...)
	p.cmd.Env = withEnv(os.Environ(), append([]string{"LOCALAPPDATA=" + p.appData}, env...)...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	p.cmd.WaitDelay = 5 * time.Second
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.stdin = stdin
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = p.cmd.Wait()
		p.code = p.cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *hostProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *hostProc) waitExit(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
		return p.code
	case <-time.After(d):
		t.Fatalf("locallm-host did not exit within %s; output:\n%s", d, p.out.String())
		return -1
	}
}

// wantStartFail — хост выходит с кодом code, в выводе есть "(exit code N)" и want; возвращает вывод.
func wantStartFail(t *testing.T, p *hostProc, code int, want string) string {
	t.Helper()
	got := p.waitExit(t, 20*time.Second)
	out := p.out.String()
	if got != code || !strings.Contains(out, fmt.Sprintf("(exit code %d)", code)) || !strings.Contains(out, want) {
		t.Fatalf("exit code %d (want %d, text %q); output:\n%s", got, code, want, out)
	}
	return out
}

var e2eClient = &http.Client{Timeout: 60 * time.Second}

type e2eHost struct {
	p    *hostProc
	dir  string // каталог конфига: keys.txt, logs\host.log
	base string // http://127.0.0.1:<порт>
	key  string // ключ из созданного хостом keys.txt
}

func (e *e2eHost) call(method, path, body, key string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, e.base+path, strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e2eClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

// startServingWith — хост с fakeupstream.exe вместо llama-server по конфигу c (пустые Dir, Listen и ServerExe
// заполняются). Ждёт /health; файла ключей нет — хост создаёт его сам, ключ читается в e.key.
func startServingWith(t *testing.T, c testConf, env ...string) *e2eHost {
	t.Helper()
	if c.Dir == "" {
		c.Dir = t.TempDir()
	}
	if c.Listen == "" {
		c.Listen = fmt.Sprintf("127.0.0.1:%d", testutil.FreePort(t))
	}
	if c.ServerExe == "" {
		c.ServerExe = testutil.BuildFakeUpstream(t)
	}
	cfg := writeTestConfig(t, c)
	e := &e2eHost{p: startHost(t, []string{"--config", cfg}, env...), dir: c.Dir, base: "http://" + c.Listen}
	schedtest.Eventually(t, 20*time.Second, func() bool {
		if e.p.exited() {
			return true
		}
		resp, _, err := e.call(http.MethodGet, "/health", "", "")
		return err == nil && resp.StatusCode == http.StatusOK
	})
	if e.p.exited() {
		t.Fatalf("locallm-host exited with code %d before serving; output:\n%s", e.p.code, e.p.out.String())
	}
	keys, err := os.ReadFile(filepath.Join(c.Dir, "keys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(keys)), "\n")
	e.key = strings.TrimSpace(lines[len(lines)-1])
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "#") || len(e.key) != 43 {
		t.Fatalf("generated keys file: %d lines, key length %d", len(lines), len(e.key))
	}
	return e
}

// startServing — модели a и b, default_model a, у b загрузка 1 с (--loading 1s), swap_cooldown 0, wait_timeout 30 s,
// vram.source none (пауза 2 с).
func startServing(t *testing.T, env ...string) *e2eHost {
	t.Helper()
	return startServingWith(t, testConf{DefaultModel: "a", ModelArgs: map[string][]string{"b": {"--loading", "1s"}}}, env...)
}

// completionTokens — POST /v1/chat/completions к модели model; usage.completion_tokens ответа
// (у fakeupstream он равен --chunks процесса, то есть показывает, с какими аргументами тот запущен).
func (e *e2eHost) completionTokens(t *testing.T, model string) int {
	t.Helper()
	resp, body, err := e.call(http.MethodPost, "/v1/chat/completions",
		`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`, e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chat %s: %v %v %s", model, err, resp, body)
	}
	var r struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("chat %s: %v %s", model, err, body)
	}
	return r.Usage.CompletionTokens
}

// modelIDs — id из GET /v1/models.
func (e *e2eHost) modelIDs(t *testing.T) []string {
	t.Helper()
	resp, body, err := e.call(http.MethodGet, "/v1/models", "", e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models: %v %v %s", err, resp, body)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("/v1/models: %v %s", err, body)
	}
	ids := []string{}
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// status — GET /api/status с ключом: поля state, model и pid (null → "" и 0).
func (e *e2eHost) status(t *testing.T) (state, model string, pid int) {
	t.Helper()
	resp, body, err := e.call(http.MethodGet, "/api/status", "", e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/status: %v %v %s", err, resp, body)
	}
	var st struct {
		State string  `json:"state"`
		Model *string `json:"model"`
		PID   *int    `json:"pid"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("/api/status: %v %s", err, body)
	}
	if st.Model != nil {
		model = *st.Model
	}
	if st.PID != nil {
		pid = *st.PID
	}
	return st.State, model, pid
}

// waitReady ждёт state=ready с моделью model и возвращает PID её процесса (fakeupstream.exe).
func (e *e2eHost) waitReady(t *testing.T, model string) int {
	t.Helper()
	pid := 0
	schedtest.Eventually(t, 20*time.Second, func() bool {
		st, m, p := e.status(t)
		pid = p
		return st == "ready" && m == model && p > 0
	})
	return pid
}

func TestE2EReleaseBuildHasNoFaultInjection(t *testing.T) {
	files := func(tags string) string {
		out, err := exec.Command("go", "list", "-tags", tags, "-f", "{{join .GoFiles \" \"}}", ".").CombinedOutput()
		if err != nil {
			t.Fatalf("go list -tags %q: %v\n%s", tags, err, out)
		}
		return string(out)
	}
	if rel := files(""); !strings.Contains(rel, "fault_none.go") || strings.Contains(rel, "fault_e2e.go") {
		t.Fatalf("release build files: %s", rel)
	}
	if e2e := files("e2efault"); !strings.Contains(e2e, "fault_e2e.go") || strings.Contains(e2e, "fault_none.go") {
		t.Fatalf("e2efault build files: %s", e2e)
	}
}

func TestE2EBadFlagExits2(t *testing.T) {
	p := startHost(t, []string{"--bogus"})
	wantStartFail(t, p, 2, "-bogus")
	b, err := os.ReadFile(filepath.Join(p.appData, "locallm", "logs", "host.log"))
	if err != nil || !strings.Contains(string(b), "exit_code=2") {
		t.Fatalf("fallback log in LOCALAPPDATA: %v\n%s", err, b)
	}
}

func TestE2EMissingConfigExits2(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	wantStartFail(t, startHost(t, []string{"--config", missing}), 2, "nope.yaml")
}

func TestE2EJobObjectFaultExits3(t *testing.T) {
	cfg := writeTestConfig(t, testConf{})
	wantStartFail(t, startHost(t, []string{"--config", cfg}, "LOCALLM_E2E_FAULT=job"), 3, "job object: e2e fault")
}

func TestE2EBadKeysFileExits4(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir})
	if err := os.WriteFile(filepath.Join(dir, "keys.txt"), []byte("tooshortkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := wantStartFail(t, startHost(t, []string{"--config", cfg}), 4, "line 1")
	log, _ := os.ReadFile(filepath.Join(dir, "logs", "host.log"))
	if strings.Contains(out, "tooshortkey") || strings.Contains(string(log), "tooshortkey") {
		t.Fatal("key value must never be shown or logged")
	}
}

func TestE2EVRAMSourceUnavailableExits5(t *testing.T) {
	cfg := writeTestConfig(t, testConf{VramSource: "nvidia-smi"})
	// PATH из пустого каталога: nvidia-smi не находится, а vram.source=nvidia-smi запрещает запасной источник.
	wantStartFail(t, startHost(t, []string{"--config", cfg}, "PATH="+t.TempDir()), 5, "vram source nvidia-smi is unavailable")
}

func TestE2EBusyListenExits6(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := writeTestConfig(t, testConf{Listen: ln.Addr().String()})
	wantStartFail(t, startHost(t, []string{"--config", cfg}), 6, "listen "+ln.Addr().String())
}

// Код 7: остановка не подтверждена во время работы (§7.4). Процесс модели a остаётся жив,
// пока хост не выйдет; затем его завершает Job Object.
func TestE2EUnconfirmedStopExits7AndJobKillsChild(t *testing.T) {
	e := startServing(t, "LOCALLM_E2E_FAULT=stop")
	pidA := e.waitReady(t, "a")
	t.Cleanup(func() { testutil.KillPID(pidA) })
	go func() { _, _, _ = e.call(http.MethodPost, "/api/swap", `{"model":"b"}`, e.key) }() // ответа не будет: хост выходит
	if code := e.p.waitExit(t, 30*time.Second); code != 7 {
		t.Fatalf("exit code %d, want 7; output:\n%s", code, e.p.out.String())
	}
	schedtest.Eventually(t, 5*time.Second, func() bool { return !testutil.ProcessAlive(pidA) })
	if out := e.p.out.String(); !strings.Contains(out, "llama-server exit not confirmed") || !strings.Contains(out, "exit_code=7") {
		t.Fatalf("output:\n%s", out)
	}
}

// Работа хоста: создание ключа, /health без ключа, 401, предзагрузка default_model, неявная пересадка
// по полю model (A5 без мака), отсутствие ключа и тела в журнале, taskkill /f → процесс модели мёртв.
func TestE2EServeImplicitSwapAndTaskkill(t *testing.T) {
	e := startServing(t)
	resp, body, err := e.call(http.MethodGet, "/health", "", "")
	if err != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("/health: %v %v %s", err, resp, body)
	}
	for _, c := range []struct{ path, key string }{
		{"/api/status", ""},
		{"/api/status", "wrong-key-wrong-key-wrong-key-wrong-key-000"},
		{"/no/such/path", ""},
	} {
		resp, body, err := e.call(http.MethodGet, c.path, "", c.key)
		if err != nil || resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"invalid_api_key"`) {
			t.Fatalf("GET %s without a valid key: %v %v %s", c.path, err, resp, body)
		}
	}
	pidA := e.waitReady(t, "a")
	t.Cleanup(func() { testutil.KillPID(pidA) })

	const marker = "e2e-request-body-marker-7f3a"
	resp, body, err = e.call(http.MethodPost, "/v1/chat/completions",
		`{"model":"b","messages":[{"role":"user","content":"`+marker+`"}]}`, e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("implicit swap a -> b: %v %v %s", err, resp, body)
	}
	waited, err := strconv.Atoi(resp.Header.Get("X-Locallm-Wait-Ms"))
	if err != nil || waited < 1000 {
		t.Fatalf("X-Locallm-Wait-Ms %q: want >= 1000 (stop a, 2 s pause, b loads 1 s)", resp.Header.Get("X-Locallm-Wait-Ms"))
	}
	var chat struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &chat); err != nil || chat.Model != "b" {
		t.Fatalf("the answer must come from model b: %v %s", err, body)
	}
	pidB := e.waitReady(t, "b")
	t.Cleanup(func() { testutil.KillPID(pidB) })
	if pidB == pidA || testutil.ProcessAlive(pidA) {
		t.Fatalf("process of a (pid %d) must exit before b (pid %d) runs", pidA, pidB)
	}
	t.Logf("implicit swap a -> b: X-Locallm-Wait-Ms=%d", waited)

	logText, err := os.ReadFile(filepath.Join(e.dir, "logs", "host.log"))
	if err != nil || !strings.Contains(string(logText), "locallm-host started") {
		t.Fatalf("host.log: %v\n%s", err, logText)
	}
	for what, secret := range map[string]string{"api key": e.key, "request body": marker} {
		if strings.Contains(string(logText), secret) || strings.Contains(e.p.out.String(), secret) {
			t.Fatalf("%s leaked into the host log", what)
		}
	}

	if out, err := exec.Command("taskkill", "/f", "/pid", strconv.Itoa(e.p.cmd.Process.Pid)).CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	e.p.waitExit(t, 10*time.Second)
	schedtest.Eventually(t, 5*time.Second, func() bool { return !testutil.ProcessAlive(pidB) })
}

// models.yaml и профиль (§5.1, §5.4 спецификации v2): каталог a, a-long, b; default_model — профиль; профиль
// запускает процесс со своими аргументами; явная пересадка на базовый вариант — другой процесс.
func TestE2EFileModeProfiles(t *testing.T) {
	e := startServingWith(t, testConf{ModelsMode: "file", DefaultModel: "a-long",
		ModelArgs: map[string][]string{"a": {"--chunks", "3"}},
		Profiles:  map[string]map[string][]string{"a": {"long": {"--chunks", "7"}}}})
	if _, err := os.Stat(filepath.Join(e.dir, "models.yaml")); err != nil {
		t.Fatal(err)
	}
	if ids := e.modelIDs(t); !slices.Equal(ids, []string{"a", "a-long", "b"}) {
		t.Fatalf("/v1/models ids %q", ids)
	}
	pidLong := e.waitReady(t, "a-long")
	t.Cleanup(func() { testutil.KillPID(pidLong) })
	if n := e.completionTokens(t, "a-long"); n != 7 {
		t.Fatalf("a-long must run with the profile arguments (--chunks 7): completion_tokens=%d", n)
	}
	resp, body, err := e.call(http.MethodPost, "/api/swap", `{"model":"a"}`, e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/swap a: %v %v %s", err, resp, body)
	}
	pidA := e.waitReady(t, "a")
	t.Cleanup(func() { testutil.KillPID(pidA) })
	if pidA == pidLong {
		t.Fatalf("a and a-long are different launches: the process must change (pid %d)", pidA)
	}
	if n := e.completionTokens(t, "a"); n != 3 {
		t.Fatalf("a must run with the model arguments (--chunks 3): completion_tokens=%d", n)
	}
	resp, body, err = e.call(http.MethodPost, "/api/swap", `{"model":"a-long"}`, e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/swap a-long: %v %v %s", err, resp, body)
	}
	pidProfile := e.waitReady(t, "a-long")
	t.Cleanup(func() { testutil.KillPID(pidProfile) })
	if pidProfile == pidA {
		t.Fatalf("swapping to a-long must replace the base process (pid %d)", pidA)
	}
	if n := e.completionTokens(t, "a-long"); n != 7 {
		t.Fatalf("a-long after /api/swap must use --chunks 7: completion_tokens=%d", n)
	}
}

// Режим file без models.yaml (§5.1 спецификации v2): хост стартует с пустым каталогом.
func TestE2EFileModeEmptyCatalog(t *testing.T) {
	e := startServingWith(t, testConf{ModelsMode: "absent"})
	if ids := e.modelIDs(t); len(ids) != 0 {
		t.Fatalf("/v1/models ids %q, want none", ids)
	}
	resp, body, err := e.call(http.MethodPost, "/api/swap", `{"model":"a"}`, e.key)
	if err != nil || resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `"model_not_found"`) {
		t.Fatalf("/api/swap with an empty catalog: %v %v %s", err, resp, body)
	}
}
