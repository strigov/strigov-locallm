// Package supervisor владеет процессом llama-server (§7): Job Object, запуск, /health, остановка, вывод.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

var _ scheduler.Backend = (*Supervisor)(nil)

type proc struct {
	cmd    *exec.Cmd
	model  string
	pid    int
	since  time.Time
	gen    uint64
	exited chan struct{} // закрывается после выхода процесса; info валидно после этого
	info   scheduler.ExitInfo
	done   chan scheduler.ExitInfo // буфер 1: одно значение, затем close (Upstream.Done)
}

type Supervisor struct {
	cfg *config.Config
	log *slog.Logger

	mu   sync.Mutex
	cur  *proc
	bufs map[string]*ringBuffer
	last string
	gen  uint64 // счётчик запусков супервизора (для строки-разделителя)

	terminate    func(*os.Process) error // тестовый шов; по умолчанию TerminateProcess через Kill
	pollInterval time.Duration           // 250 мс (§7.3)
	portWait     time.Duration           // 5 с (§7.2 п. 1)
	health       *http.Client
}

func New(cfg *config.Config, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Supervisor{
		cfg:          cfg,
		log:          log,
		bufs:         map[string]*ringBuffer{},
		terminate:    func(p *os.Process) error { return p.Kill() },
		pollInterval: 250 * time.Millisecond,
		portWait:     5 * time.Second,
		health:       &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
	}
}

// Start — §4.4 / §7.2–7.3.
func (s *Supervisor) Start(ctx context.Context, m config.Model) (*scheduler.Upstream, error) {
	if st := s.Status(); st.Running {
		return nil, fmt.Errorf("llama-server is already running (model %q, pid %d)", st.Model, st.PID)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.Llama.Port))
	if err := s.waitPortFree(ctx, addr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// load_timeout истёк во время ожидания порта: это таймаут, а не port_busy (§7.2 п. 1).
			s.log.Error("llama-server start cancelled while waiting for port", "model", m.ID, "addr", addr, "err", err)
			return nil, &LoadError{Reason: "timeout", ExitCode: -1, Tail: []string{err.Error()}}
		}
		s.log.Error("llama-server port busy", "model", m.ID, "addr", addr, "err", err)
		return nil, &LoadError{Reason: "port_busy", ExitCode: -1, Tail: []string{err.Error()}}
	}
	p, err := s.spawn(m)
	if err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "http", Host: addr}
	if err := s.waitReady(ctx, p, u); err != nil {
		return nil, err
	}
	s.log.Info("llama-server ready", "model", m.ID, "pid", p.pid, "gen", p.gen, "load_ms", time.Since(p.since).Milliseconds())
	return scheduler.NewUpstream(m.ID, u, p.pid, p.done), nil
}

func (s *Supervisor) waitPortFree(ctx context.Context, addr string) error {
	deadline := time.Now().Add(s.portWait)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is in use by another process: %v", addr, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s (last error: %v): %w", addr, err, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// childEnv исключает настройки llama-server, которыми управляет хост.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if name != "LLAMA_API_KEY" && !strings.HasPrefix(name, "LLAMA_ARG_") {
			out = append(out, entry)
		}
	}
	return out
}

func (s *Supervisor) spawn(m config.Model) (*proc, error) {
	argv := s.cfg.Argv(m)
	redact, lookahead := argumentRedactor(s.cfg.Llama.DefaultArgs, m.Args)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Dir(s.cfg.Llama.ServerExe)
	cmd.Env = childEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, &LoadError{Reason: "spawn", ExitCode: -1, Tail: []string{err.Error()}}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, &LoadError{Reason: "spawn", ExitCode: -1, Tail: []string{err.Error()}}
	}
	cmd.Stdout, cmd.Stderr = outW, errW

	s.mu.Lock()
	if s.cur != nil {
		s.mu.Unlock()
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, errors.New("llama-server is already running")
	}
	s.gen++
	gen := s.gen
	buf := s.bufs[m.ID]
	if buf == nil {
		buf = newRingBuffer(s.cfg.Llama.LogLines)
		s.bufs[m.ID] = buf
	}
	s.mu.Unlock()

	startErr := cmd.Start()
	outW.Close()
	errW.Close()
	if startErr != nil {
		outR.Close()
		errR.Close()
		s.log.Error("llama-server spawn failed", "model", m.ID, "err", startErr)
		return nil, &LoadError{Reason: "spawn", ExitCode: -1, Tail: []string{startErr.Error()}}
	}
	p := &proc{cmd: cmd, model: m.ID, pid: cmd.Process.Pid, since: time.Now(), gen: gen,
		exited: make(chan struct{}), done: make(chan scheduler.ExitInfo, 1)}
	buf.Add(fmt.Sprintf("--- start %s pid=%d gen=%d ---", time.Now().UTC().Format(time.RFC3339), p.pid, gen))
	mark := buf.Mark()
	s.mu.Lock()
	s.cur, s.last = p, m.ID
	s.mu.Unlock()

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); readLines(outR, buf, redact, lookahead) }()
	go func() { defer readers.Done(); readLines(errR, buf, redact, lookahead) }()
	go s.wait(p, buf, mark, &readers)
	s.log.Info("llama-server started", "model", m.ID, "pid", p.pid, "gen", gen)
	return p, nil
}

// argumentFlagRE — токен, который llama.cpp прочитает как имя флага: длинный флаг в kebab- или snake-написании
// (llama.cpp заменяет '_' на '-' у аргументов с "--", F2-D1), с "=value" или без, либо короткий "-x=...".
// Самостоятельный snake-токен после флага также рассматривается как возможное значение (F1-B1).
var argumentFlagRE = regexp.MustCompile(`^--[a-z0-9_-]+(=.*)?$|^-[^=]+=`)

// argumentRedactor скрывает длинные потенциально секретные значения default_args/args
// (пары flag value и --flag=value), включая -private-leading-secret и --Pr1vate_... .
// Числа и значения короче 8 байт сохраняются. Следующий флаг обрабатывается отдельно.
// Lowercase-токен вида --x_y после флага маскируется как возможное значение и также считается флагом:
// следующий токен рассматривается как его значение. Правило действует только при наличии '_' и без '='.
// Ограничение F3-D3: отдельное lowercase-значение вида --x-y считается флагом и не маскируется.
func argumentRedactor(argGroups ...[]string) (*strings.Replacer, int) {
	var values []string
	for _, args := range argGroups {
		for i := 0; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "-") {
				if j := strings.IndexByte(a, '='); j >= 0 {
					a = a[j+1:]
				} else if i+1 < len(args) && !argumentFlagRE.MatchString(args[i+1]) {
					i++
					a = args[i]
				} else if i+1 < len(args) && strings.Contains(args[i+1], "_") && !strings.Contains(args[i+1], "=") {
					a = args[i+1]
				} else {
					continue
				}
			}
			if len(a) < 8 {
				continue
			}
			if _, err := strconv.ParseFloat(a, 64); err == nil || errors.Is(err, strconv.ErrRange) {
				continue
			}
			values = append(values, a)
		}
	}
	// Сначала длинные значения: короткое значение может быть префиксом секрета.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	lookahead := 0
	for _, value := range values {
		// Не укорачивать строку: иначе обрезанный конец lookahead может
		// сдвинуться в сохраняемый префикс и раскрыть часть следующего значения.
		mask := "[redacted]" + strings.Repeat("*", max(0, len(value)-len("[redacted]")))
		pairs = append(pairs, value, mask)
		lookahead = max(lookahead, len(value)-1)
	}
	return strings.NewReplacer(pairs...), lookahead
}

// readLines всегда дочитывает пайп, сохраняя только ограниченный префикс строки.
// Lookahead скрывает и значение, пересекающее границу обрезки в 2 КиБ.
func readLines(r *os.File, buf *ringBuffer, redact *strings.Replacer, lookahead int) {
	defer r.Close()
	br := bufio.NewReaderSize(r, 4096)
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 && len(line) < maxLineBytes+lookahead {
			line = append(line, chunk[:min(len(chunk), maxLineBytes+lookahead-len(line))]...)
		}
		if err != nil {
			if len(line) > 0 {
				buf.Add(redact.Replace(string(line)))
			}
			return
		}
		if !isPrefix {
			buf.Add(redact.Replace(strings.TrimRight(string(line), "\r")))
			line = line[:0]
		}
	}
}

func (s *Supervisor) wait(p *proc, buf *ringBuffer, mark uint64, readers *sync.WaitGroup) {
	_ = p.cmd.Wait()
	code := -1
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select { // дочитать хвост; не дольше 1 с, если пайп унаследовал внук
	case <-drained:
	case <-time.After(time.Second):
	}
	p.info = scheduler.ExitInfo{Code: code, At: time.Now(), Tail: buf.Since(mark, 20, 4096)}
	s.mu.Lock()
	if s.cur == p {
		s.cur = nil
	}
	s.mu.Unlock()
	close(p.exited)
	p.done <- p.info
	close(p.done)
	s.log.Info("llama-server exited", "model", p.model, "pid", p.pid, "gen", p.gen, "exit_code", code)
}

// Stop — §7.4: до трёх попыток TerminateProcess с ожиданием stop_timeout.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	p := s.cur
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return s.stopProc(ctx, p)
}

func (s *Supervisor) stopProc(ctx context.Context, p *proc) error {
	for attempt := 1; attempt <= 3; attempt++ {
		if err := s.terminate(p.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			s.log.Warn("TerminateProcess failed", "model", p.model, "pid", p.pid, "gen", p.gen, "attempt", attempt, "err", err)
		}
		t := time.NewTimer(s.cfg.Llama.StopTimeout)
		select {
		case <-p.exited:
			t.Stop()
			return nil
		case <-ctx.Done():
			t.Stop()
			s.log.Error("llama-server stop interrupted; exit not confirmed", "model", p.model, "pid", p.pid, "gen", p.gen)
			return scheduler.ErrStopUnconfirmed
		case <-t.C:
			s.log.Warn("llama-server did not exit within stop_timeout", "model", p.model, "pid", p.pid, "gen", p.gen, "attempt", attempt)
		}
	}
	s.log.Error("llama-server exit not confirmed after 3 attempts", "model", p.model, "pid", p.pid, "gen", p.gen)
	return scheduler.ErrStopUnconfirmed
}

func (s *Supervisor) Status() scheduler.BackendStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return scheduler.BackendStatus{}
	}
	return scheduler.BackendStatus{Running: true, Model: s.cur.model, PID: s.cur.pid, Since: s.cur.since}
}

// Logs — последние tail строк буфера модели; model "" — последняя запускавшаяся модель.
// ok == false — модель ещё не запускалась.
func (s *Supervisor) Logs(model string, tail int) ([]string, bool) {
	s.mu.Lock()
	if model == "" {
		model = s.last
	}
	buf := s.bufs[model]
	s.mu.Unlock()
	if buf == nil {
		return nil, false
	}
	return buf.Last(tail), true
}

// ForgetLogs забывает буферы вывода имён ids (§6.8 спецификации v2: имена, удалённые из каталога), кроме буфера
// работающего процесса: его пишут горутины чтения вывода. Забыт буфер последней запускавшейся модели — Logs("")
// больше его не отдаёт.
func (s *Supervisor) ForgetLogs(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if s.cur != nil && s.cur.model == id {
			continue
		}
		delete(s.bufs, id)
		if s.last == id {
			s.last = ""
		}
	}
}
