package tray

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Action — что выбрал человек в меню (§11.1).
type Action int

const (
	ActLoad      Action = iota + 1 // пункт подменю «Загрузить»; Click.Model — имя
	ActMore                        // «Остальные — в панели»
	ActUnload                      // «Выгрузить модель (освободить видеокарту)»
	ActForce                       // «Прервать запросы и выгрузить»
	ActPause                       // «Пауза»
	ActResume                      // «Продолжить»
	ActPanel                       // «Открыть панель»
	ActLog                         // «Открыть журнал»
	ActFolder                      // «Открыть папку настроек»
	ActAutostart                   // флажок «Запускать при входе в Windows»; Click.Checked — состояние до щелчка
	ActQuit                        // «Выход»
	ActLang                        // пункт подменю «Язык»; Click.Lang — выбранный язык
)

// String — имя действия для журнала.
func (a Action) String() string {
	switch a {
	case ActLoad:
		return "load"
	case ActMore:
		return "more"
	case ActUnload:
		return "unload"
	case ActForce:
		return "force_unload"
	case ActPause:
		return "pause"
	case ActResume:
		return "resume"
	case ActPanel:
		return "panel"
	case ActLog:
		return "log"
	case ActFolder:
		return "folder"
	case ActAutostart:
		return "autostart"
	case ActQuit:
		return "quit"
	case ActLang:
		return "language"
	}
	return "unknown"
}

// Click — щелчок по пункту меню.
type Click struct {
	Action  Action
	Model   string
	Checked bool
	Lang    i18n.Lang
}

// entry — пункт и то, что драйвер уже показывает: Apply передаёт драйверу только изменения (§11.2). У скрытого пункта
// меняется только видимость: fyne.io/systray показывает скрытый пункт при любом другом изменении.
type entry struct {
	it      Item
	title   string
	enabled bool
	visible bool
}

func newEntry(it Item, title string) *entry {
	return &entry{it: it, title: title, enabled: true, visible: true}
}

func (e *entry) set(title string, enabled, visible bool) {
	if visible != e.visible {
		if visible {
			e.it.Show()
		} else {
			e.it.Hide()
		}
		e.visible = visible
	}
	if !e.visible {
		return
	}
	if title != e.title {
		e.it.SetTitle(title)
		e.title = title
	}
	if enabled != e.enabled {
		if enabled {
			e.it.Enable()
		} else {
			e.it.Disable()
		}
		e.enabled = enabled
	}
}

// slot — пункт подменю «Загрузить»; id — имя, которое он сейчас показывает ("" — пункт скрыт).
type slot struct {
	*entry
	id string
}

// Menu — меню трея над Driver (§11.1, §11.2).
type Menu struct {
	d       Driver
	onClick func(Click)

	mu        sync.Mutex
	lang      i18n.Lang
	iconSet   bool
	icon      Color
	tooltip   string
	paused    bool // пункт паузы показывает «Продолжить»
	auto      bool // флажок автозапуска, как показан
	status    *entry
	load      *entry
	unload    *entry
	force     *entry
	pause     *entry
	panel     *entry
	logItem   *entry
	folder    *entry
	autostart *entry
	language  *entry
	langs     []Item // пункты подменю «Язык» в порядке i18n.All
	quit      *entry
	slots     []*slot
	more      *entry // nil, пока имён не больше MaxLoadItems
}

// NewMenu создаёт пункты верхнего уровня в порядке §11.1 на языке lang и горутины щелчков. Вызывается после того,
// как драйвер создал значок (onReady). onClick вызывается в новой горутине на каждый щелчок. Стрелку подменю у
// «Загрузить» и «Язык» рисует Windows.
func NewMenu(d Driver, lang i18n.Lang, onClick func(Click)) *Menu {
	m := &Menu{d: d, onClick: onClick, lang: lang}
	add := func(k i18n.Key) *entry { return newEntry(d.AddItem(lang.T(k)), lang.T(k)) }
	m.status = newEntry(d.AddItem("locallm-host"), "locallm-host")
	m.status.set("locallm-host", false, true)
	d.AddSeparator()
	m.load = add(i18n.MenuLoad)
	m.unload = add(i18n.MenuUnload)
	m.force = add(i18n.MenuForce)
	m.pause = add(i18n.MenuPause)
	d.AddSeparator()
	m.panel = add(i18n.MenuPanel)
	m.logItem = add(i18n.MenuLog)
	m.folder = add(i18n.MenuFolder)
	m.autostart = newEntry(d.AddCheckbox(lang.T(i18n.MenuAutostart), false), lang.T(i18n.MenuAutostart))
	m.language = add(i18n.MenuLanguage)
	for _, l := range i18n.All {
		it := m.language.it.AddSubCheckbox(l.Native(), l == lang)
		m.langs = append(m.langs, it)
		m.watch(it, func() Click { return Click{Action: ActLang, Lang: l} })
	}
	d.AddSeparator()
	m.quit = add(i18n.MenuQuit)
	m.watch(m.unload.it, func() Click { return Click{Action: ActUnload} })
	m.watch(m.force.it, func() Click { return Click{Action: ActForce} })
	m.watch(m.pause.it, func() Click {
		if m.paused {
			return Click{Action: ActResume}
		}
		return Click{Action: ActPause}
	})
	m.watch(m.panel.it, func() Click { return Click{Action: ActPanel} })
	m.watch(m.logItem.it, func() Click { return Click{Action: ActLog} })
	m.watch(m.folder.it, func() Click { return Click{Action: ActFolder} })
	m.watch(m.autostart.it, func() Click { return Click{Action: ActAutostart, Checked: m.auto} })
	m.watch(m.quit.it, func() Click { return Click{Action: ActQuit} })
	return m
}

// Lang — язык, на котором показано меню.
func (m *Menu) Lang() i18n.Lang {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lang
}

// SetLang — меню на языке l: заголовки постоянных пунктов меняются сразу, галочка — у выбранного языка; строка
// состояния и подсказка — при следующем Apply. Тот же язык — драйвер не вызывается.
func (m *Menu) SetLang(l i18n.Lang) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l == m.lang {
		return
	}
	for i, it := range m.langs {
		switch i18n.All[i] {
		case l:
			it.Check()
		case m.lang:
			it.Uncheck()
		}
	}
	m.lang = l
	pause := i18n.MenuPause
	if m.paused {
		pause = i18n.MenuResume
	}
	for _, e := range []struct {
		e *entry
		k i18n.Key
	}{{m.load, i18n.MenuLoad}, {m.unload, i18n.MenuUnload}, {m.force, i18n.MenuForce}, {m.pause, pause},
		{m.panel, i18n.MenuPanel}, {m.logItem, i18n.MenuLog}, {m.folder, i18n.MenuFolder},
		{m.autostart, i18n.MenuAutostart}, {m.language, i18n.MenuLanguage}, {m.quit, i18n.MenuQuit}} {
		e.e.set(l.T(e.k), e.e.enabled, true)
	}
	if m.more != nil {
		m.more.set(l.T(i18n.MenuMore), m.more.enabled, m.more.visible)
	}
}

// watch — §11.2: горутина на пункт читает его канал щелчков; действие запускается в новой горутине. mk вызывается под
// m.mu; Click с нулевым Action (скрытый пункт подменю) ничего не делает.
func (m *Menu) watch(it Item, mk func() Click) {
	ch := it.Clicked()
	go func() {
		for range ch {
			m.mu.Lock()
			c := mk()
			m.mu.Unlock()
			if c.Action != 0 {
				go m.onClick(c)
			}
		}
	}()
}

// Apply показывает вид v, передавая драйверу только изменения; значок — только при смене цвета (§11.2).
func (m *Menu) Apply(v MenuView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.iconSet || v.Icon != m.icon {
		m.d.SetIcon(v.Icon)
		m.icon, m.iconSet = v.Icon, true
	}
	if v.Tooltip != m.tooltip {
		m.d.SetTooltip(v.Tooltip)
		m.tooltip = v.Tooltip
	}
	l := m.lang
	m.status.set(v.Status, false, true)
	m.load.set(l.T(i18n.MenuLoad), v.CanLoad, true)
	m.unload.set(l.T(i18n.MenuUnload), v.CanUnload, true)
	m.force.set(l.T(i18n.MenuForce), v.CanForce, true)
	m.paused = v.Paused
	pause := i18n.MenuPause
	if v.Paused {
		pause = i18n.MenuResume
	}
	m.pause.set(l.T(pause), true, true)
	for i, li := range v.Load {
		if i == len(m.slots) {
			s := &slot{entry: newEntry(m.load.it.AddSubItem(li.Title), li.Title)}
			m.slots = append(m.slots, s)
			m.watch(s.it, func() Click {
				if s.id == "" {
					return Click{}
				}
				return Click{Action: ActLoad, Model: s.id}
			})
		}
		m.slots[i].id = li.ID
		m.slots[i].set(li.Title, true, true)
	}
	for _, s := range m.slots[len(v.Load):] {
		s.id = ""
		s.set(s.title, s.enabled, false)
	}
	if v.More && m.more == nil { // создаётся после всех пунктов имён и потому остаётся последним
		m.more = newEntry(m.load.it.AddSubItem(l.T(i18n.MenuMore)), l.T(i18n.MenuMore))
		m.watch(m.more.it, func() Click { return Click{Action: ActMore} })
	}
	if m.more != nil {
		m.more.set(l.T(i18n.MenuMore), true, v.More)
	}
}

// SetAutostart — флажок «Запускать при входе в Windows»; без изменения драйвер не вызывается.
func (m *Menu) SetAutostart(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on == m.auto {
		return
	}
	if on {
		m.autostart.it.Check()
	} else {
		m.autostart.it.Uncheck()
	}
	m.auto = on
}

// Source — откуда Follow берёт данные.
type Source struct {
	Snapshot  func() *scheduler.Snapshot
	VRAM      func(ctx context.Context) (int64, error) // nil — VRAM не показывается
	Autostart func() (bool, error)                     // nil — флажок не обновляется
}

// Интервалы §11.2; тесты пакета уменьшают.
var (
	snapshotEvery  = 500 * time.Millisecond
	vramEvery      = 2 * time.Second
	vramTimeout    = time.Second
	autostartEvery = 10 * time.Second
)

// Follow — горутина обновления (§11.2) до отмены ctx: Snapshot раз в snapshotEvery; VRAM раз в vramEvery в своей
// горутине со сроком vramTimeout (ошибка или срок — VRAM в подсказке не показывается); автозапуск раз в autostartEvery
// (ошибка чтения — флажок прежний). Первое обновление — сразу. Интервалы читаются один раз, при входе.
func (m *Menu) Follow(ctx context.Context, src Source) {
	snapEvery, vEvery, vTimeout, autoEvery := snapshotEvery, vramEvery, vramTimeout, autostartEvery
	var vram atomic.Int64
	vram.Store(-1)
	if src.VRAM != nil {
		go func() {
			for {
				vctx, cancel := context.WithTimeout(ctx, vTimeout)
				v, err := src.VRAM(vctx)
				cancel()
				if err != nil {
					v = -1
				}
				vram.Store(v)
				select {
				case <-ctx.Done():
					return
				case <-time.After(vEvery):
				}
			}
		}()
	}
	update := func() { m.Apply(View(src.Snapshot(), vram.Load(), time.Now(), m.Lang())) }
	autostart := func() {
		if src.Autostart == nil {
			return
		}
		if on, err := src.Autostart(); err == nil {
			m.SetAutostart(on)
		}
	}
	update()
	autostart()
	st := time.NewTicker(snapEvery)
	defer st.Stop()
	at := time.NewTicker(autoEvery)
	defer at.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.C:
			update()
		case <-at.C:
			autostart()
		}
	}
}
