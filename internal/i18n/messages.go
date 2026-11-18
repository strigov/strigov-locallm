package i18n

// Key — текст трея или окна.
type Key int

const (
	// Пункты меню трея (§11.1).
	MenuLoad Key = iota
	MenuUnload
	MenuForce
	MenuPause
	MenuResume
	MenuPanel
	MenuLog
	MenuFolder
	MenuAutostart
	MenuQuit
	MenuMore
	MenuLanguage

	// Строка состояния (§11.1).
	StatusHalted
	StatusLoading      // %s — модель
	StatusReady        // %s — модель
	StatusAnswering    // %d — незаконченные ответы; добавляется к StatusReady
	StatusDraining     // %s → %s
	StatusDrainUnload  // цель Draining без новой модели
	StatusDrainRestart // цель Draining — та же модель
	StatusStopping     // %s — модель
	StatusWaitVRAM
	StatusEmpty
	StatusLoadFailed // %s — модель
	StatusPaused     // %s — остальная строка

	// Подсказка значка (§11.1).
	TipMain   // %s — модель, %d — запросы
	TipNone   // модели нет
	TipVRAM   // %s — ГБ
	TipPaused // добавляется в конце

	// Окна.
	ConfirmForce    // %d — незаконченные ответы
	ConfirmQuit     // %d — незаконченные ответы и ожидающие
	AutostartFailed // %s — ошибка
	NotResponding
	BrowserFailed // %s — адрес панели

	keyCount
)

var ru = [keyCount]string{
	MenuLoad:      "Загрузить",
	MenuUnload:    "Выгрузить модель (освободить видеокарту)",
	MenuForce:     "Прервать запросы и выгрузить",
	MenuPause:     "Пауза",
	MenuResume:    "Продолжить",
	MenuPanel:     "Открыть панель",
	MenuLog:       "Открыть журнал",
	MenuFolder:    "Открыть папку настроек",
	MenuAutostart: "Запускать при входе в Windows",
	MenuQuit:      "Выход",
	MenuMore:      "Остальные — в панели",
	MenuLanguage:  "Язык · Language",

	StatusHalted:       "Ошибка: хост завершается",
	StatusLoading:      "Загружается: %s…",
	StatusReady:        "Готова: %s",
	StatusAnswering:    " · отвечает (%d)",
	StatusDraining:     "Заканчивает ответы: %s → %s",
	StatusDrainUnload:  "выгрузка",
	StatusDrainRestart: "перезапуск",
	StatusStopping:     "Выгружается: %s…",
	StatusWaitVRAM:     "Освобождается видеопамять…",
	StatusEmpty:        "Модель не загружена",
	StatusLoadFailed:   "Ошибка загрузки: %s",
	StatusPaused:       "Пауза · %s",

	TipMain:   "locallm: %s · запросов: %d",
	TipNone:   "пусто",
	TipVRAM:   " · VRAM %s ГБ",
	TipPaused: " · пауза",

	ConfirmForce:    "Будут прерваны незаконченные ответы: %d. Продолжить?",
	ConfirmQuit:     "Прервать незаконченные ответы (%d) и выйти?",
	AutostartFailed: "Не удалось изменить автозапуск: %s",
	NotResponding:   "locallm-host уже запущен, но не отвечает. Если он завис, завершите его в Диспетчере задач и запустите снова",
	BrowserFailed:   "Не удалось открыть браузер. Откройте панель вручную: %s",
}

var en = [keyCount]string{
	MenuLoad:      "Load",
	MenuUnload:    "Unload model (free the GPU)",
	MenuForce:     "Abort requests and unload",
	MenuPause:     "Pause",
	MenuResume:    "Resume",
	MenuPanel:     "Open panel",
	MenuLog:       "Open log",
	MenuFolder:    "Open settings folder",
	MenuAutostart: "Start when signing in to Windows",
	MenuQuit:      "Exit",
	MenuMore:      "More in the panel",
	MenuLanguage:  "Language",

	StatusHalted:       "Error: host is shutting down",
	StatusLoading:      "Loading: %s…",
	StatusReady:        "Ready: %s",
	StatusAnswering:    " · answering (%d)",
	StatusDraining:     "Finishing responses: %s → %s",
	StatusDrainUnload:  "unload",
	StatusDrainRestart: "restart",
	StatusStopping:     "Unloading: %s…",
	StatusWaitVRAM:     "Freeing video memory…",
	StatusEmpty:        "No model loaded",
	StatusLoadFailed:   "Load failed: %s",
	StatusPaused:       "Paused · %s",

	TipMain:   "locallm: %s · requests: %d",
	TipNone:   "none",
	TipVRAM:   " · VRAM %s GB",
	TipPaused: " · paused",

	ConfirmForce:    "Unfinished responses will be aborted: %d. Continue?",
	ConfirmQuit:     "Abort unfinished responses (%d) and exit?",
	AutostartFailed: "Could not change autostart: %s",
	NotResponding:   "locallm-host is already running but does not respond. If it is frozen, end it in Task Manager and start it again",
	BrowserFailed:   "Could not open the browser. Open the panel manually: %s",
}

var zh = [keyCount]string{
	MenuLoad:      "加载",
	MenuUnload:    "卸载模型（释放显卡）",
	MenuForce:     "中断请求并卸载",
	MenuPause:     "暂停",
	MenuResume:    "继续",
	MenuPanel:     "打开面板",
	MenuLog:       "打开日志",
	MenuFolder:    "打开设置文件夹",
	MenuAutostart: "登录 Windows 时启动",
	MenuQuit:      "退出",
	MenuMore:      "其余模型请在面板中查看",
	MenuLanguage:  "语言 · Language",

	StatusHalted:       "错误：主机正在退出",
	StatusLoading:      "正在加载：%s…",
	StatusReady:        "就绪：%s",
	StatusAnswering:    " · 正在应答（%d）",
	StatusDraining:     "正在完成应答：%s → %s",
	StatusDrainUnload:  "卸载",
	StatusDrainRestart: "重启",
	StatusStopping:     "正在卸载：%s…",
	StatusWaitVRAM:     "正在释放显存…",
	StatusEmpty:        "未加载模型",
	StatusLoadFailed:   "加载失败：%s",
	StatusPaused:       "已暂停 · %s",

	TipMain:   "locallm: %s · 请求: %d",
	TipNone:   "无",
	TipVRAM:   " · 显存 %s GB",
	TipPaused: " · 已暂停",

	ConfirmForce:    "将中断未完成的应答：%d 个。是否继续？",
	ConfirmQuit:     "中断未完成的应答（%d 个）并退出？",
	AutostartFailed: "无法更改自动启动：%s",
	NotResponding:   "locallm-host 已在运行，但没有响应。如果它已卡住，请在任务管理器中结束它，然后重新启动",
	BrowserFailed:   "无法打开浏览器。请手动打开面板：%s",
}
