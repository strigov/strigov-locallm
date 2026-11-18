package control

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
)

// ErrPanelUnavailable — у контроллера нет входа (Options.Auth) или запуска браузера (Options.OpenURL).
var ErrPanelUnavailable = errors.New("panel is not available: auth or browser launcher is not configured")

// PanelError — браузер не открылся (§11.3). URL — адрес панели без кода: его можно показать человеку в окне.
type PanelError struct {
	URL string
	Err error
}

func (e *PanelError) Error() string { return "cannot open the browser: " + e.Err.Error() }
func (e *PanelError) Unwrap() error { return e.Err }

// OpenPanel — §9.4: одноразовый код и браузер на http://<server.listen>/ui/#otp=<код>[&lang=<язык трея>] (решение S9:
// фрагмент доходит до страницы). Код не попадает ни в журнал, ни в текст ошибки.
func (c *Controller) OpenPanel() error {
	if c.auth == nil || c.openURL == nil {
		return ErrPanelUnavailable
	}
	base, err := panelURL(c.live.Load().Server.Listen)
	if err != nil {
		return err
	}
	target := base + "#otp=" + c.auth.MintOTP(c.now())
	if c.lang != nil {
		if l := c.lang(); l != "" {
			target += "&lang=" + url.QueryEscape(l)
		}
	}
	if err := c.openURL(target); err != nil {
		return &PanelError{URL: base, Err: err}
	}
	return nil
}

// panelURL — адрес панели из server.listen: IPv6 в скобках; «любой адрес» (пусто, 0.0.0.0, ::) заменяется петлёй
// той же версии — по нему браузер не откроет страницу.
func panelURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", errors.New("server.listen is not host:port")
	}
	if host == "" {
		host = "127.0.0.1"
	} else if a, perr := netip.ParseAddr(host); perr == nil && a.IsUnspecified() {
		host = "127.0.0.1"
		if a.Is6() {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/ui/", nil
}
