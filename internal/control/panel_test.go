package control

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// §9.4: адрес панели из server.listen — IPv6 в скобках, «любой адрес» заменяется петлёй той же версии.
func TestPanelURL(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/ui/"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080/ui/"},
		{":8080", "http://127.0.0.1:8080/ui/"},
		{"[::1]:8080", "http://[::1]:8080/ui/"},
		{"[::]:8080", "http://[::1]:8080/ui/"},
		{"100.64.0.7:8080", "http://100.64.0.7:8080/ui/"},
		{"localhost:9000", "http://localhost:9000/ui/"},
		{"[fd7a:115c:a1e0::1]:8080", "http://[fd7a:115c:a1e0::1]:8080/ui/"},
	} {
		if got, err := panelURL(tc.listen); err != nil || got != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.listen, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:"} {
		if got, err := panelURL(bad); err == nil {
			t.Errorf("%q: %q accepted", bad, got)
		}
	}
}

func panelController(listen string, open func(string) error) (*Controller, *Auth) {
	cfg := &config.Config{}
	cfg.Server.Listen = listen
	auth := NewAuth(nil, nil)
	return New(Options{Live: config.NewLive(cfg), Auth: auth, OpenURL: open}), auth
}

// §9.4, решение S9: OpenPanel открывает /ui/#otp=<код>; код из 43 символов base64url работает в Login один раз.
func TestOpenPanelOpensOTPURL(t *testing.T) {
	var opened []string
	ctl, auth := panelController("[::1]:18080", func(u string) error { opened = append(opened, u); return nil })
	if err := ctl.OpenPanel(); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^http://\[::1\]:18080/ui/#otp=([A-Za-z0-9_-]{43})$`).FindStringSubmatch(strings.Join(opened, "\n"))
	if m == nil {
		t.Fatalf("opened %q", opened)
	}
	if _, _, err := auth.Login(m[1], "", time.Now()); err != nil {
		t.Fatalf("code from the panel address: %v", err)
	}
	if _, _, err := auth.Login(m[1], "", time.Now()); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("second use of the code: %v", err)
	}
}

// Options.Lang: язык трея уходит в адрес панели после кода (&lang=en) — панель открывается на том же языке.
func TestOpenPanelPassesLanguage(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:18080"
	var opened string
	ctl := New(Options{Live: config.NewLive(cfg), Auth: NewAuth(nil, nil), Lang: func() string { return "en" },
		OpenURL: func(u string) error { opened = u; return nil }})
	if err := ctl.OpenPanel(); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^http://127.0.0.1:18080/ui/#otp=[A-Za-z0-9_-]{43}&lang=en$`).MatchString(opened) {
		t.Fatalf("opened %q", opened)
	}
}

// §11.3: браузер не открылся — PanelError с адресом панели без кода; кода нет и в тексте ошибки.
func TestOpenPanelBrowserFailure(t *testing.T) {
	noBrowser := errors.New("no default browser")
	var opened string
	ctl, _ := panelController("127.0.0.1:18080", func(u string) error { opened = u; return noBrowser })
	err := ctl.OpenPanel()
	var pe *PanelError
	if !errors.As(err, &pe) || !errors.Is(err, noBrowser) || pe.URL != "http://127.0.0.1:18080/ui/" {
		t.Fatalf("err %v", err)
	}
	code := strings.TrimPrefix(opened, "http://127.0.0.1:18080/ui/#otp=")
	if len(code) != 43 || strings.Contains(err.Error(), code) || strings.Contains(err.Error(), "otp") {
		t.Fatalf("error text %q must not contain the code", err.Error())
	}
}

func TestOpenPanelUnavailable(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:18080"
	if err := New(Options{Live: config.NewLive(cfg)}).OpenPanel(); !errors.Is(err, ErrPanelUnavailable) {
		t.Fatalf("err %v", err)
	}
}
