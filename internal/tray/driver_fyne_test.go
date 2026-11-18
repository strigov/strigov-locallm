//go:build windows

package tray

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// F1-D3 (ledger первого плана): строки fyne.io/systray из стандартного log уходят в журнал хоста; первая строка
// об ошибке создания окна или меню запоминается для Run.
func TestLogBridge(t *testing.T) {
	var buf bytes.Buffer
	d := &fyneDriver{log: slog.New(slog.NewTextHandler(&buf, nil))}
	b := logBridge{d}
	_, _ = b.Write([]byte("systray error: unable to set tooltip: x\n"))
	if d.initErr.Load() != nil {
		t.Fatal("a tooltip error is not an init failure")
	}
	_, _ = b.Write([]byte("systray error: unable to init instance: Access is denied.\n"))
	_, _ = b.Write([]byte("systray error: unable to create menu: later\n"))
	if p := d.initErr.Load(); p == nil || *p != "unable to init instance: Access is denied." {
		t.Fatalf("init error %v", p)
	}
	log := buf.String()
	if strings.Count(log, `msg="tray error"`) != 3 || !strings.Contains(log, `err="unable to set tooltip: x"`) ||
		strings.Contains(log, "systray error:") {
		t.Fatalf("log:\n%s", log)
	}
}

// F2-B1: onReady не стартует раньше первого значка; при ошибке значка или после Quit onReady не вызывается.
func TestOnReadyWaitsForFirstIcon(t *testing.T) {
	d := &fyneDriver{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	// run — обёртка в горутине библиотеки: вернёт, вызвался ли onReady после сигнала в iconReady.
	run := func(err error) bool {
		iconReady := make(chan error, 1)
		ran := make(chan struct{}, 1)
		go d.gatedOnReady(iconReady, func() { ran <- struct{}{} })()
		iconReady <- err
		select {
		case <-ran:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}
	if run(errors.New("boom")) {
		t.Fatal("onReady called despite the first icon error")
	}
	if !run(nil) {
		t.Fatal("onReady not called after the first icon succeeded")
	}
	d.Quit()
	if run(nil) {
		t.Fatal("onReady called after Quit")
	}
}

// Quit до Run: Run сразу возвращается без значка (systray.Register не вызывается).
func TestQuitBeforeRun(t *testing.T) {
	d := NewFyneDriver(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	d.Quit()
	done := make(chan error, 1)
	go func() { done <- d.Run(func() { t.Error("onReady after Quit") }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run after Quit did not return")
	}
	d.Quit() // повторный вызов ничего не делает
}
