package desktop

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	testExe    = `C:\Apps\locallm\locallm-host.exe`
	testConfig = `C:\Users\me\AppData\Local\locallm\locallm.yaml`
)

// testAutostart — разделы HKCU\Software\locallm-test\<уникальное имя>\Run и …\StartupApproved\Run; удаляются в Cleanup.
// Настоящие разделы автозапуска не трогаются.
func testAutostart(t *testing.T) Autostart {
	t.Helper()
	base := `Software\locallm-test\` + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	a := Autostart{RunKey: base + `\Run`, ApprovedKey: base + `\StartupApproved\Run`, Name: "locallm-host", Exe: testExe, Config: testConfig}
	t.Cleanup(func() {
		for _, k := range []string{a.ApprovedKey, base + `\StartupApproved`, a.RunKey, base, `Software\locallm-test`} {
			_ = registry.DeleteKey(registry.CURRENT_USER, k) // последний — только если пуст
		}
	})
	return a
}

func setString(t *testing.T, key, name, value string) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, value); err != nil {
		t.Fatal(err)
	}
}

func setBinary(t *testing.T, key, name string, value []byte) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetBinaryValue(name, value); err != nil {
		t.Fatal(err)
	}
}

// valueKind — тип значения или 0, если значения нет.
func valueKind(t *testing.T, key, name string) uint32 {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	_, kind, err := k.GetValue(name, nil)
	if errors.Is(err, registry.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func enabled(t *testing.T, a Autostart) bool {
	t.Helper()
	on, err := a.Enabled()
	if err != nil {
		t.Fatal(err)
	}
	return on
}

// §11.4: включить — значение Run (REG_SZ) с кавычками; выключить — оба значения удалены; повторное выключение — не
// ошибка; чужие значения в разделе не трогаются.
func TestAutostartEnableDisable(t *testing.T) {
	a := testAutostart(t)
	if enabled(t, a) {
		t.Fatal("no keys: enabled")
	}
	setString(t, a.RunKey, "other-app", `"C:\Other\app.exe"`)
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, a.RunKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, kind, err := k.GetStringValue(a.Name)
	k.Close()
	want := fmt.Sprintf(`"%s" --config "%s"`, testExe, testConfig)
	if err != nil || kind != registry.SZ || v != want || a.Command() != want {
		t.Fatalf("Run value %q (kind %d, err %v), want %q", v, kind, err, want)
	}
	if !enabled(t, a) {
		t.Fatal("after Enable: disabled")
	}
	if err := a.Disable(); err != nil {
		t.Fatal(err)
	}
	if enabled(t, a) || valueKind(t, a.RunKey, a.Name) != 0 || valueKind(t, a.ApprovedKey, a.Name) != 0 {
		t.Fatal("after Disable: values left")
	}
	if valueKind(t, a.RunKey, "other-app") != registry.SZ {
		t.Fatal("Disable removed a foreign value")
	}
	if err := a.Disable(); err != nil {
		t.Fatalf("second Disable: %v", err)
	}
}

// §11.4: отметка Диспетчера задач «выключено» (младший бит первого байта 1) снимает флажок; «включено» (0) — нет;
// Enable удаляет только нашу отметку.
func TestAutostartStartupApproved(t *testing.T) {
	a := testAutostart(t)
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	off := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	setBinary(t, a.ApprovedKey, a.Name, off)
	setBinary(t, a.ApprovedKey, "other-app", off)
	if enabled(t, a) {
		t.Fatal("disabled in Task Manager: checkbox must be off")
	}
	setBinary(t, a.ApprovedKey, a.Name, []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if !enabled(t, a) {
		t.Fatal("enabled in Task Manager: checkbox must be on")
	}
	setBinary(t, a.ApprovedKey, a.Name, off)
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	if !enabled(t, a) || valueKind(t, a.ApprovedKey, a.Name) != 0 {
		t.Fatal("Enable must delete our StartupApproved value")
	}
	if valueKind(t, a.ApprovedKey, "other-app") != registry.BINARY {
		t.Fatal("Enable removed a foreign StartupApproved value")
	}
}

// §11.4: путь exe в значении Run (первая строка в кавычках) сравнивается с текущим без учёта регистра; путь конфига
// не сравнивается; значение без кавычек — не наше.
func TestAutostartForeignExe(t *testing.T) {
	a := testAutostart(t)
	for _, tc := range []struct {
		value string
		on    bool
	}{
		{`"C:\Other\locallm-host.exe" --config "C:\x.yaml"`, false},
		{`"c:\apps\LOCALLM\locallm-host.exe" --config "C:\y.yaml"`, true},
		{`C:\Apps\locallm\locallm-host.exe --config C:\x.yaml`, false},
		{`"C:\Apps\locallm\locallm-host.exe`, false},
	} {
		setString(t, a.RunKey, a.Name, tc.value)
		if got := enabled(t, a); got != tc.on {
			t.Errorf("%s: enabled %v, want %v", tc.value, got, tc.on)
		}
	}
}

// §11.4: разделы и имя настоящего автозапуска (значения не читаются и не пишутся).
func TestDefaultAutostartKeys(t *testing.T) {
	a := DefaultAutostart(testExe, testConfig)
	if a.RunKey != `Software\Microsoft\Windows\CurrentVersion\Run` ||
		a.ApprovedKey != `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run` ||
		a.Name != "locallm-host" || a.Exe != testExe || a.Config != testConfig {
		t.Fatalf("%+v", a)
	}
}

// F5-B1: если удалить значение Run не удалось, нашу отметку StartupApproved не трогаем — иначе «выключить» сняло бы
// отметку, а команда осталась бы в Run, и автозапуск включился бы снова. Раздел Run с компонентом длиннее 255 символов
// не открывается (ERROR_INVALID_PARAMETER — подтверждено пробным запуском: OpenKey возвращает «The parameter is
// incorrect.», а не ErrNotExist), поэтому deleteValue(RunKey) детерминированно падает; под HKCU\Software\locallm-test\
// ничего не создаётся.
func TestAutostartDisableRunFailureKeepsStartupApproved(t *testing.T) {
	base := `Software\locallm-test\` + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	a := Autostart{
		RunKey:      base + `\` + strings.Repeat("x", 300),
		ApprovedKey: base + `\StartupApproved\Run`,
		Name:        "locallm-host",
		Exe:         testExe,
		Config:      testConfig,
	}
	t.Cleanup(func() {
		for _, k := range []string{a.ApprovedKey, base + `\StartupApproved`, base, `Software\locallm-test`} {
			_ = registry.DeleteKey(registry.CURRENT_USER, k) // последний — только если пуст
		}
	})
	off := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	setBinary(t, a.ApprovedKey, a.Name, off)
	if err := a.Disable(); err == nil {
		t.Fatal("Disable: want an error from the failed Run delete")
	}
	if kind := valueKind(t, a.ApprovedKey, a.Name); kind != registry.BINARY {
		t.Fatalf("StartupApproved value kind %d after the failed Run delete; want BINARY (untouched)", kind)
	}
}
