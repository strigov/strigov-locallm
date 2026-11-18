// Command locallm-host — хост locallm: планировщик моделей, HTTP-шлюз перед llama-server и трей (спецификация §4.7).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/desktop"
	"github.com/strigov/strigov-locallm/internal/gateway"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/logging"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/vram"
)

var version = "dev" // -ldflags "-X main.version=0.2.4"

// instanceWait — сколько второй экземпляр ждёт ответа первого (§11.5); тесты уменьшают.
var instanceWait = 10 * time.Second

// sys — внешние действия хоста; тесты подменяют их.
type sys struct {
	stderr   io.Writer
	enterJob func() error
	detect   func(ctx context.Context, source string, log *slog.Logger) (vram.Probe, error)
	dialog   func(text string)
	exit     func(code int)
	backend  func(cfg *config.Config, log *slog.Logger) (scheduler.Backend, gateway.LogSource)
	instance func(name string) (inst *desktop.Instance, first bool, err error) // шаг 1a (§11.5)
	openURL  func(log *slog.Logger, url string) error                          // браузер (§11.3)
	tray     func(h *host) error                                               // шаг 10: значок в главном потоке; nil — без трея
	testQuit func(quit func())                                                 // сквозные тесты: штатный выход без трея (fault_e2e.go)

	confirm    func(text string) bool                    // окно «Да/Нет» из трея (§11.2)
	openFile   func(log *slog.Logger, path string) error // Блокнот (§11.3)
	openFolder func(log *slog.Logger, dir string) error  // Проводник (§11.3)

	lang     func() i18n.Lang        // язык окон и трея при старте; nil — прежний (тесты: русский)
	saveLang func(l i18n.Lang) error // сохранить выбор из подменю «Язык»; nil — не сохраняется
}

func defaultSys() sys {
	return sys{
		stderr:   os.Stderr,
		enterJob: supervisor.EnterJob,
		detect:   vram.Detect,
		dialog:   showDialog,
		exit:     os.Exit,
		backend: func(cfg *config.Config, log *slog.Logger) (scheduler.Backend, gateway.LogSource) {
			sup := supervisor.New(cfg, log)
			return sup, sup
		},
		instance:   desktop.AcquireInstance,
		openURL:    desktop.OpenURL,
		tray:       runTray,
		confirm:    confirmDialog,
		openFile:   desktop.OpenFile,
		openFolder: desktop.OpenFolder,
		lang:       func() i18n.Lang { return i18n.Startup(i18n.DefaultPath()) },
		saveLang:   func(l i18n.Lang) error { return i18n.Save(i18n.DefaultPath(), l) },
	}
}

func main() {
	s := defaultSys()
	applyFaults(&s)
	os.Exit(run(os.Args[1:], s))
}

// host — состояние процесса между шагами старта, треем и выходом.
type host struct {
	sys      sys
	noDialog bool
	log      *slog.Logger
	logPath  string
	srv      *http.Server
	code     chan int // §11.6: код выхода; буфер 1, побеждает первый инициатор

	fatal   atomic.Bool // onFatal начался: штатный выход код не отправляет
	exiting atomic.Bool // штатный выход начался

	trayMu     sync.Mutex
	trayQuit   func() // убрать значок (Driver.Quit)
	trayClosed bool   // значок убран; трей больше не создаётся

	// Заполняются на шагах 1a–7; ими пользуются трей и штатный выход.
	ctx         context.Context
	cfg         *config.Config
	cfgAbs      string
	sched       *scheduler.Scheduler
	probe       vram.Probe
	ctl         *control.Controller
	ctlShutdown func(ctx context.Context, closeServer func()) error
}

// run — порядок старта §4.7; возвращает код выхода.
func run(args []string, s sys) int {
	h := &host{sys: s, noDialog: hasFlag(args, "no-dialog"), code: make(chan int, 1)}
	if s.lang != nil { // до askFirst: его окно — уже на выбранном языке
		i18n.Set(s.lang())
	}
	fs := flag.NewFlagSet("locallm-host", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "path to locallm.yaml")
	logStderr := fs.Bool("log-stderr", false, "also write the log to stderr")
	noTray := fs.Bool("no-tray", false, "do not create the tray icon (tests and debugging)")
	fs.BoolVar(&h.noDialog, "no-dialog", h.noDialog, "never show a message box")
	if err := fs.Parse(args); err != nil { // шаг 1
		return h.startFail(2, fmt.Errorf("flags: %w", err))
	}
	if fs.NArg() > 0 {
		return h.startFail(2, fmt.Errorf("flags: unexpected arguments %q", fs.Args()))
	}
	cfgAbs, err := filepath.Abs(*cfgPath) // шаг 1a: экземпляр — по абсолютному пути конфига (§11.5)
	if err != nil {
		return h.startFail(2, fmt.Errorf("config path: %w", err))
	}
	h.cfgAbs = cfgAbs
	inst, first, instErr := s.instance(desktop.InstanceName(cfgAbs))
	if instErr == nil && !first {
		return h.askFirst(inst)
	}
	if inst != nil {
		defer inst.Close()
	}
	cfg, err := config.Load(*cfgPath) // шаг 2
	if err != nil {
		return h.startFail(2, err) // config.Load уже начинает текст с «config <путь>:»
	}
	cfg.Version = version
	log, closer, err := logging.Open(cfg.Logging.Dir, cfg.Logging.Level, cfg.Logging.MaxSizeMiB, cfg.Logging.MaxFiles, *logStderr) // шаг 3
	if err != nil {
		return h.startFail(2, fmt.Errorf("log %s: %w", cfg.Logging.Dir, err))
	}
	defer closer.Close()
	h.log, h.logPath, h.cfg = log, filepath.Join(cfg.Logging.Dir, "host.log"), cfg
	if instErr != nil { // §11.5: хост работает без защиты от второго экземпляра
		log.Warn("single instance check is unavailable; running without it", "err", instErr)
	}
	// Паника или фатальная ошибка runtime пишется в crash.log: у сборки без консоли stderr пропадает.
	if cf, err := logging.OpenCrashFile(cfg.Logging.Dir, 1<<20); err != nil {
		log.Warn("crash output is unavailable", "err", err)
	} else {
		if err := debug.SetCrashOutput(cf, debug.CrashOptions{}); err != nil {
			log.Warn("crash output is unavailable", "err", err)
		} else {
			defer debug.SetCrashOutput(nil, debug.CrashOptions{}) // отпустить файл: тесты запускают run в своём процессе
		}
		cf.Close() // SetCrashOutput держит свою копию дескриптора
	}
	for _, w := range cfg.Warnings { // §5.2 спецификации v2: например, недоступный каталог models_dirs
		log.Warn("config warning", "detail", w)
	}
	if err := s.enterJob(); err != nil { // шаг 4: KILL_ON_JOB_CLOSE и BREAKAWAY_OK (§7.1)
		return h.startFail(3, fmt.Errorf("job object: %w", err))
	}
	keys, err := config.LoadOrCreateKeys(cfg.Server.APIKeysFile, log) // шаг 5
	if err != nil {
		return h.startFail(4, err)
	}
	adminKeys, err := config.LoadOrCreateAdminKeys(cfg.Server.AdminKeyFile, keys, log) // шаг 5: ключ администратора (§5.10 v2)
	if err != nil {
		return h.startFail(4, err)
	}
	paused := readPaused(cfg.Server.StateFile, log) // шаг 5a
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe, err := s.detect(ctx, cfg.Vram.Source, log) // шаг 6
	if err != nil {
		return h.startFail(5, fmt.Errorf("vram: %w", err))
	}
	backend, logs := s.backend(cfg, log) // шаг 7
	live := config.NewLive(cfg)          // каталог на ходу меняет контроллер (§4.4 v2)
	sched := scheduler.New(cfg, backend, probe, scheduler.Init{Paused: paused}, h.fatalRuntime, log)
	cache := gguf.NewCache(0)
	forget, _ := logs.(control.LogForgetter) // *supervisor.Supervisor умеет ForgetLogs
	auth := control.NewAuth(adminKeys, log)
	ctl := control.New(control.Options{Live: live, Sched: sched, Logs: forget, Cache: cache,
		Scanner: gguf.NewScanner(cache, gguf.DefaultLimits, nil), Log: log,
		Auth: auth, OpenURL: func(url string) error { return s.openURL(log, url) },
		Lang: func() string { return string(i18n.Current()) }})
	gw := gateway.New(live, sched, logs, probe, keys, log, gateway.WithAdmin(ctl, auth), gateway.WithHeaders(cache))
	h.srv = gateway.NewHTTPServer(gw, log)
	h.ctx, h.sched, h.probe, h.ctl, h.ctlShutdown = ctx, sched, probe, ctl, ctl.Shutdown
	go sched.Run(ctx)
	ctl.Warm(nil, cfg)                              // фоновое чтение заголовков GGUF моделей каталога (§8.3)
	ln, err := net.Listen("tcp", cfg.Server.Listen) // шаг 8
	if err != nil {
		return h.startFail(6, fmt.Errorf("listen %s: %w", cfg.Server.Listen, err))
	}
	log.Info("locallm-host started", "version", version, "config", cfg.Path, "models", strings.Join(cfg.ModelIDs(), ","),
		"vram.source", probe.Name(), "listen", ln.Addr().String(), "llama.port", cfg.Llama.Port, "paused", paused)
	preloadDefault(log, cfg.Swap.DefaultModel, paused, sched.Load) // шаг 9
	go h.serve(ln, cfg.Server.Listen)                              // шаг 10
	if inst != nil {
		go inst.Serve(h.panelRequest)
	}
	if s.testQuit != nil {
		s.testQuit(func() { h.shutdown("test") })
	}
	if !*noTray && s.tray != nil {
		if err := s.tray(h); err != nil { // ошибка значка не фатальна (§4.7)
			log.Warn("tray is unavailable; running without it", "err", err)
		}
	}
	return <-h.code
}

// hasFlag — булев флаг до разбора: --no-dialog действует и при ошибке разбора флагов.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a = strings.TrimLeft(a, "-"); a == name || a == name+"=true" {
			return true
		}
	}
	return false
}

// readPaused — шаг 5a §4.7: пауза из state.json (§5.10 v2). Файла нет — пауза снята. Файл не читается или испорчен —
// пауза включена и предупреждение в журнале: хост не знает, была ли пауза, и сам модели не грузит.
func readPaused(path string, log *slog.Logger) bool {
	st, exists, err := config.ReadState(path)
	if err != nil {
		log.Warn("state file is unreadable; starting paused", "path", path, "err", err)
		return true
	}
	return exists && st.Paused
}

// preloadDefault — шаг 9 §4.7: swap.default_model грузится, только если она задана и пауза не включена. На паузе —
// строка журнала. Ошибка загрузки не фатальна.
func preloadDefault(log *slog.Logger, model string, paused bool, load func(string) error) {
	switch {
	case model == "":
	case paused:
		log.Info("default model skipped: host is paused", "model", model)
	default:
		if err := load(model); err != nil {
			log.Error("default model preload failed", "model", model, "err", err)
		}
	}
}

// askFirst — шаг 1a §4.7 у второго экземпляра: попросить первый открыть панель (§11.5). Ответил — код 0; не ответил за
// instanceWait — код 6 и окно на русском (§3.3). Журнал ещё не открыт: строка уходит в FallbackPath.
func (h *host) askFirst(inst *desktop.Instance) int {
	defer inst.Close()
	if inst.Ask(instanceWait) {
		return 0
	}
	h.reportFail(6, fmt.Errorf("another locallm-host with this config is running but did not answer within %s", instanceWait))
	if !h.noDialog {
		h.sys.dialog(i18n.Current().T(i18n.NotResponding))
	}
	return 6
}

// serve — Serve в своей горутине (шаг 10). ErrServerClosed — сервер закрыли штатный выход или onFatal; другая ошибка
// фатальна, как в MVP (код 6).
func (h *host) serve(ln net.Listener, listen string) {
	if err := h.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		h.removeIcon()
		h.finish(h.startFail(6, fmt.Errorf("serve %s: %w", listen, err)))
	}
}

// finish — §11.6: код выхода в канал; побеждает первый инициатор, остальные отбрасываются. Значок убирается.
func (h *host) finish(code int) {
	select {
	case h.code <- code:
	default:
	}
	h.removeIcon()
}

// setTrayQuit — трей сообщает, как убрать значок. false — значок уже убран (хост выходит), трей не создаётся.
func (h *host) setTrayQuit(quit func()) bool {
	h.trayMu.Lock()
	defer h.trayMu.Unlock()
	if h.trayClosed {
		return false
	}
	h.trayQuit = quit
	return true
}

// removeIcon — убрать значок один раз; после этого трей не создаётся.
func (h *host) removeIcon() {
	h.trayMu.Lock()
	quit := h.trayQuit
	h.trayQuit, h.trayClosed = nil, true
	h.trayMu.Unlock()
	if quit != nil {
		quit()
	}
}

// shutdown — штатный выход (§11.6, шаги 2–5): трей «Выход» или тестовый хук; окно подтверждения (шаг 1) — у трея, до
// вызова. Halted во время ожидания — побеждает onFatal (код 7). Повторный вызов ничего не делает.
func (h *host) shutdown(reason string) {
	if !h.exiting.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*h.cfg.Llama.StopTimeout+2*time.Second)
	defer cancel()
	err := h.ctlShutdown(ctx, func() { _ = h.srv.Close() })
	if h.fatal.Load() || errors.Is(err, scheduler.ErrHalted) {
		return // onFatal уберёт значок, покажет окно и отправит код 7
	}
	if err != nil {
		h.log.Warn("shutdown did not finish cleanly; exiting anyway", "err", err)
	}
	h.log.Info("locallm-host exiting", "reason", reason)
	h.finish(0)
}

// openPanel — «Открыть панель» в трее и просьба второго экземпляра. Браузер не открылся — строка журнала и окно
// с адресом панели без кода (§11.3). Окно — в своей горутине: ответ второму экземпляру (shown) не ждёт его закрытия.
func (h *host) openPanel() {
	err := h.ctl.OpenPanel()
	if err == nil {
		return
	}
	h.log.Warn("panel was not opened", "err", err)
	var pe *control.PanelError
	if errors.As(err, &pe) && !h.noDialog {
		go h.sys.dialog(i18n.Current().F(i18n.BrowserFailed, pe.URL))
	}
}

// panelRequest — §11.5: второй экземпляр попросил панель.
func (h *host) panelRequest() {
	h.log.Info("second instance asked for the panel")
	h.openPanel()
}

// reportFail — фатальная ошибка: журнал (до его открытия — FallbackPath) и stderr; возвращает путь журнала для окна.
func (h *host) reportFail(code int, err error) string {
	logPath := h.logPath
	if h.log != nil {
		h.log.Error("locallm-host failed to start", "exit_code", code, "err", err)
	} else {
		logPath = logging.FallbackPath()
		if f, ferr := logging.NewRotatingFile(logPath, 10<<20, 5); ferr == nil {
			slog.New(slog.NewTextHandler(f, nil)).Error("locallm-host failed to start", "exit_code", code, "err", err)
			_ = f.Close()
		}
	}
	fmt.Fprintf(h.sys.stderr, "locallm-host: %v (exit code %d)\n", err, code)
	return logPath
}

// startFail — фатальная ошибка старта (§4.7): reportFail и MessageBoxW, как в MVP.
func (h *host) startFail(code int, err error) int {
	logPath := h.reportFail(code, err)
	if !h.noDialog {
		h.sys.dialog(fmt.Sprintf("locallm-host failed to start (exit code %d):\n\n%v\n\nLog: %s", code, err, logPath))
	}
	return code
}

// fatalRuntime — onFatal планировщика (§4.7): неподтверждённая остановка llama-server во время работы. Автомат уже
// в Halted: новый llama-server не запустится, пока открыто окно. Значок убирается до окна.
func (h *host) fatalRuntime(err error) {
	h.fatal.Store(true)
	h.log.Error("llama-server exit not confirmed; locallm-host is exiting", "exit_code", 7, "err", err)
	h.removeIcon()
	_ = h.srv.Close()
	if !h.noDialog {
		h.sys.dialog(fmt.Sprintf("locallm-host stopped (exit code 7):\n\n%v\n\nLog: %s", err, h.logPath))
	}
	h.finish(7)
	h.sys.exit(7)
}
