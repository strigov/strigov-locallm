// Package vram измеряет занятую видеопамять (§8): PDH → nvidia-smi → пауза.
package vram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// Probe — источник замера занятой видеопамяти.
type Probe interface {
	Name() string                                // "pdh" | "nvidia-smi" | "none"
	UsageMiB(ctx context.Context) (int64, error) // сумма Dedicated Usage, МиБ
}

var ErrUnavailable = errors.New("vram usage unavailable")

type openers struct {
	pdh func() (Probe, error)
	smi func(ctx context.Context) (Probe, error)
}

var system = openers{pdh: openPDH, smi: openNvidiaSMI}

// Detect выбирает источник по vram.source (§8.1). "pdh" и "nvidia-smi" — только этот источник,
// недоступность — ошибка (хост выходит с кодом 5). "auto" (и "") пробует PDH → nvidia-smi → none,
// на каждый пропуск — warn с причиной. Выбранный источник пишется в журнал.
func Detect(ctx context.Context, source string, log *slog.Logger) (Probe, error) {
	return detect(ctx, source, log, system)
}

func detect(ctx context.Context, source string, log *slog.Logger, o openers) (Probe, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var p Probe
	var err error
	switch source {
	case "pdh":
		if p, err = o.pdh(); err != nil {
			return nil, fmt.Errorf("vram source pdh is unavailable: %w", err)
		}
	case "nvidia-smi":
		if p, err = o.smi(ctx); err != nil {
			return nil, fmt.Errorf("vram source nvidia-smi is unavailable: %w", err)
		}
	case "none":
		log.Warn("vram source none: WaitVRAM is a fixed 2s pause")
		p = none{}
	case "auto", "":
		if p, err = o.pdh(); err != nil {
			log.Warn("vram source pdh skipped", "reason", err)
			if p, err = o.smi(ctx); err != nil {
				log.Warn("vram source nvidia-smi skipped", "reason", err)
				log.Warn("vram source none: WaitVRAM is a fixed 2s pause")
				p = none{}
			}
		}
	default:
		return nil, fmt.Errorf("unknown vram source %q", source)
	}
	log.Info("vram source selected", "source", p.Name())
	return p, nil
}
