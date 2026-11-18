package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// §11.5, §14.2: второй экземпляр с тем же конфигом просит первый открыть панель и выходит с кодом 0; первый открывает
// http://<listen>/ui/#otp=<код> (фейковый браузер сборки e2efault), и код меняется на сессию. Кода нет в журнале.
func TestE2ESecondInstanceOpensPanel(t *testing.T) {
	urls := filepath.Join(t.TempDir(), "urls.txt")
	dir := t.TempDir()
	e := startServingWith(t, testConf{Dir: dir}, "LOCALLM_E2E_OPENURL="+urls)
	second := startHost(t, []string{"--config", filepath.Join(dir, "locallm.yaml")})
	if code := second.waitExit(t, 20*time.Second); code != 0 {
		t.Fatalf("second instance: exit code %d; output:\n%s", code, second.out.String())
	}
	data, err := os.ReadFile(urls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	m := regexp.MustCompile(`^` + regexp.QuoteMeta(e.base) + `/ui/#otp=([A-Za-z0-9_-]{43})&lang=(ru|en|zh)$`).FindStringSubmatch(lines[0])
	if len(lines) != 1 || m == nil {
		t.Fatalf("opened addresses: %q", lines)
	}
	resp, body, err := e.call(http.MethodPost, "/api/admin/session", `{"otp":"`+m[1]+`"}`, "")
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"token"`) {
		t.Fatalf("login with the code: %v %v %s", err, resp, body)
	}
	if out := e.p.out.String(); !strings.Contains(out, `msg="second instance asked for the panel"`) || strings.Contains(out, m[1]) {
		t.Fatalf("first instance log must have the request and never the code:\n%s", out)
	}
}

// §11.6, §14.2: штатный выход через тестовый хук — код 0, процесс модели (fakeupstream) завершён, state.json не записан.
func TestE2EQuitExits0AndStopsModel(t *testing.T) {
	e := startServing(t, "LOCALLM_E2E_QUIT=stdin")
	pid := e.waitReady(t, "a")
	t.Cleanup(func() { testutil.KillPID(pid) })
	if _, err := io.WriteString(e.p.stdin, "quit\n"); err != nil {
		t.Fatal(err)
	}
	if code := e.p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0; output:\n%s", code, e.p.out.String())
	}
	schedtest.Eventually(t, 5*time.Second, func() bool { return !testutil.ProcessAlive(pid) })
	if out := e.p.out.String(); !strings.Contains(out, `msg="locallm-host exiting" reason=test`) {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json on exit: %v", err)
	}
}
