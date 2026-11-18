// Package control — контроллер хоста (§4.4 спецификации v2): команды автомату от имени трея и администратора,
// пауза с state.json, сохранение моделей, вид состояния, поиск файлов и вход в панель.
package control

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Кто отдал команду (§13: поле actor).
const (
	KindTray  = "tray"
	KindAdmin = "admin_api"
)

// Actor — источник команды. Session — первые 8 hex SHA-256 токена сессии; "" — трей или ключ администратора.
type Actor struct {
	Kind    string
	Session string
}

// Scheduler — то, что контроллер берёт у планировщика; *scheduler.Scheduler его реализует.
type Scheduler interface {
	Load(model string) error
	Unload(model string) error
	ForceUnload(model string) error
	Restart(model string) error
	Pause(force bool) error
	Resume() error
	Reconfigure(catalog []config.Model, guard bool) error
	Snapshot() *scheduler.Snapshot
}

var _ Scheduler = (*scheduler.Scheduler)(nil)

// LogForgetter — забыть буферы вывода удалённых имён (*supervisor.Supervisor).
type LogForgetter interface{ ForgetLogs(ids []string) }

// Options — зависимости контроллера. Logs, Cache, Scanner, Log, Auth и OpenURL могут быть nil.
type Options struct {
	Live    *config.Live
	Sched   Scheduler
	Logs    LogForgetter           // nil — буферы вывода не чистятся
	Cache   *gguf.Cache            // nil — без info, предупреждений о заголовках и фонового чтения
	Scanner *gguf.Scanner          // nil — Scan отдаёт пустой результат
	Log     *slog.Logger           // nil — журнал отбрасывается
	Auth    *Auth                  // nil — OpenPanel возвращает ErrPanelUnavailable
	OpenURL func(url string) error // nil — то же; в адресе одноразовый код: реализация не пишет его в журнал и в ошибку
	Lang    func() string          // язык трея для адреса панели (&lang=); nil или "" — без языка
}

// Controller — §4.4. Изменения моделей сериализует modelsMu (§4.6), паузу — pauseMu; мьютексы независимы.
type Controller struct {
	live    *config.Live
	sched   Scheduler
	logs    LogForgetter
	cache   *gguf.Cache
	scanner *gguf.Scanner
	log     *slog.Logger

	auth    *Auth
	openURL func(url string) error
	lang    func() string

	// Точки подмены (тесты пакета).
	now               func() time.Time
	writeFile         func(path string, data []byte, ifMatch string) (etag, backup string, err error)
	writeState        func(path string, st config.HostState) error
	migrate           func(c *config.Config) (*config.Config, config.MigrateResult, error)
	beforeReconfigure func() // между шагами 7 и 8 PUT (тест H17 (а))
	warnTimeout       time.Duration
	shutdownPoll      time.Duration // шаг ожидания фазы в Shutdown

	pauseMu    sync.Mutex
	pauseSaved atomic.Bool // последняя запись state.json удалась (pause_saved, §9.5)
	modelsMu   sync.Mutex

	diskMu sync.Mutex
	disk   diskStamp // ETag models.yaml на диске при известных размере и времени записи

	usedSem chan struct{} // ёмкость 1: не больше одного расчёта used_by сразу (usedByWait)
}

// New — контроллер; pause_saved изначально true (state.json прочитан при старте или его нет).
func New(o Options) *Controller {
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Controller{live: o.Live, sched: o.Sched, logs: o.Logs, cache: o.Cache, scanner: o.Scanner, log: log,
		auth: o.Auth, openURL: o.OpenURL, lang: o.Lang,
		now: time.Now, writeFile: config.WriteFileAtomic, writeState: config.WriteState, migrate: config.Migrate,
		beforeReconfigure: func() {}, warnTimeout: 3 * time.Second, shutdownPoll: 50 * time.Millisecond,
		usedSem: make(chan struct{}, 1)}
	c.pauseSaved.Store(true)
	return c
}

// Load — явная загрузка (C2–C4). Ошибки автомата — как есть (ErrModelNotFound, ErrPaused, ErrHalted). Строка журнала
// пишется после успешной команды: имя к этому моменту проверено автоматом (§13).
func (c *Controller) Load(a Actor, model string) error {
	if err := c.sched.Load(model); err != nil {
		return err
	}
	c.logCommand(a, "load", model, false)
	return nil
}

// Unload — выгрузка (C5–C8); force — с обрывом начатых ответов (F1); model "" — текущая.
func (c *Controller) Unload(a Actor, model string, force bool) error {
	if !force {
		if err := c.sched.Unload(model); err != nil {
			return err
		}
		c.logCommand(a, "unload", model, false)
		return nil
	}
	if err := c.sched.ForceUnload(model); err != nil {
		return err
	}
	c.logCommand(a, "unload", model, true)
	return nil
}

// Restart — перезапуск с текущим определением (C9–C12).
func (c *Controller) Restart(a Actor, model string) error {
	if err := c.sched.Restart(model); err != nil {
		return err
	}
	c.logCommand(a, "restart", model, false)
	return nil
}

// Pause — пауза (C13); force — с обрывом начатых ответов. persisted == false — state.json не записан (пауза в памяти
// действует до перезапуска).
func (c *Controller) Pause(a Actor, force bool) (bool, error) { return c.setPaused(a, true, force) }

// Resume — снятие паузы (C14).
func (c *Controller) Resume(a Actor) (bool, error) { return c.setPaused(a, false, false) }

// setPaused — §4.4: под pauseMu команда автомату, затем запись state.json. Поэтому записи идут в порядке команд,
// и последняя запись совпадает с Snapshot().Paused. Ошибка записи не отменяет паузу: persisted == false.
func (c *Controller) setPaused(a Actor, paused, force bool) (bool, error) {
	c.pauseMu.Lock()
	defer c.pauseMu.Unlock()
	var err error
	action := "resume"
	if paused {
		action = "pause"
		err = c.sched.Pause(force)
	} else {
		err = c.sched.Resume()
	}
	if err != nil {
		return false, err
	}
	c.logCommand(a, action, "", force)
	path := c.live.Load().Server.StateFile
	var werr error
	if filepath.IsAbs(path) {
		werr = c.writeState(path, config.HostState{Paused: paused, ChangedAt: c.now().UTC(), By: a.Kind})
	} else {
		werr = errors.New("server.state_file is not set") // такой конфиг Load не даёт; не пишем в текущий каталог
	}
	c.pauseSaved.Store(werr == nil)
	if werr != nil {
		c.log.Warn("pause state not saved", "paused", paused, "persisted", false, "err", werr)
		return false, nil
	}
	c.log.Info("pause state saved", "paused", paused, "persisted", true)
	return true, nil
}

// logCommand — строка журнала команды (§13): actor, session, model, force; тела запросов не пишутся.
// Строку F1 («forced unload») пишет автомат (forceRetire): она появляется только когда обрыв действительно был.
func (c *Controller) logCommand(a Actor, action, model string, force bool) {
	c.log.Info("admin command", "action", action, "actor", a.Kind, "session", a.Session, "model", model, "force", force)
}
