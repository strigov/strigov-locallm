package tray

// Item — пункт меню (§11.2). Пункты не удаляются: только скрываются и показываются. Clicked получает щелчки; канал
// читает одна горутина Menu.
type Item interface {
	SetTitle(title string)
	Enable()
	Disable()
	Check()
	Uncheck()
	Show()
	Hide()
	Clicked() <-chan struct{}
	AddSubItem(title string) Item
	AddSubCheckbox(title string, checked bool) Item
}

// Driver — узкий интерфейс значка в трее (§11.2). Run вызывается в главном потоке ОС и крутит цикл сообщений до Quit;
// onReady вызывается в своей горутине, когда значок создан. Ошибка Run — значок не создан. Quit безопасен до, во время
// и после Run; повторный вызов ничего не делает.
type Driver interface {
	Run(onReady func()) error
	Quit()
	SetIcon(c Color)
	SetTooltip(text string)
	AddItem(title string) Item
	AddCheckbox(title string, checked bool) Item
	AddSeparator()
}
