//go:build e2efault

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gateway"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// applyFaults — внедрение отказов и тестовые хуки для сквозных тестов. Файл собирается только с тегом e2efault;
// release-сборка (scripts\build.ps1) содержит fault_none.go.
//
// Всегда в этой сборке: значка в трее нет (tray = nil), браузер не запускается — адрес дописывается строкой в файл
// LOCALLM_E2E_OPENURL (без переменной — ошибка «browser is disabled in e2e builds»).
//
//	LOCALLM_E2E_QUIT=stdin — строка "quit" в stdin запускает штатный выход (§11.6) с reason=test.
//	LOCALLM_E2E_FAULT=job  — вход в Job Object «не удался» → код выхода 3;
//	LOCALLM_E2E_FAULT=stop — Stop бэкенда сообщает ErrStopUnconfirmed и процесс не останавливает → код выхода 7,
//	                          а живой llama-server (в тестах fakeupstream.exe) завершает Job Object при выходе хоста.
//
// Любое другое значение (и пустое) ничего не меняет.
func applyFaults(s *sys) {
	s.tray = nil
	s.openURL = func(_ *slog.Logger, url string) error {
		path := os.Getenv("LOCALLM_E2E_OPENURL")
		if path == "" {
			return errors.New("browser is disabled in e2e builds")
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(f, url)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	if os.Getenv("LOCALLM_E2E_QUIT") == "stdin" {
		s.testQuit = func(quit func()) {
			go func() {
				sc := bufio.NewScanner(os.Stdin)
				for sc.Scan() {
					if strings.TrimSpace(sc.Text()) == "quit" {
						quit()
						return
					}
				}
			}()
		}
	}
	switch os.Getenv("LOCALLM_E2E_FAULT") {
	case "job":
		s.enterJob = func() error { return errors.New("e2e fault: job object unavailable") }
	case "stop":
		next := s.backend
		s.backend = func(cfg *config.Config, log *slog.Logger) (scheduler.Backend, gateway.LogSource) {
			b, logs := next(cfg, log)
			return stopNeverConfirms{b}, logs
		}
	}
}

// stopNeverConfirms — Backend, у которого остановка не подтверждается (§7.4): процесс остаётся жив.
type stopNeverConfirms struct{ scheduler.Backend }

func (stopNeverConfirms) Stop(context.Context) error { return scheduler.ErrStopUnconfirmed }
