package vram

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type namedProbe string

func (n namedProbe) Name() string                            { return string(n) }
func (n namedProbe) UsageMiB(context.Context) (int64, error) { return 100, nil }

func testOpeners(pdhErr, smiErr error) (openers, *int, *int) {
	pdhCalls, smiCalls := new(int), new(int)
	return openers{
		pdh: func() (Probe, error) {
			*pdhCalls++
			if pdhErr != nil {
				return nil, pdhErr
			}
			return namedProbe("pdh"), nil
		},
		smi: func(context.Context) (Probe, error) {
			*smiCalls++
			if smiErr != nil {
				return nil, smiErr
			}
			return namedProbe("nvidia-smi"), nil
		},
	}, pdhCalls, smiCalls
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestDetectAuto(t *testing.T) {
	pdhDown, smiDown := errors.New("pdh counter missing"), errors.New("nvidia-smi not found")
	cases := []struct {
		name           string
		pdhErr, smiErr error
		want           string
		warns          int
		reasons        []string
	}{
		{"pdh first", nil, nil, "pdh", 0, nil},
		{"fallback to nvidia-smi", pdhDown, nil, "nvidia-smi", 1, []string{"pdh counter missing"}},
		{"fallback to none", pdhDown, smiDown, "none", 3, []string{"pdh counter missing", "nvidia-smi not found"}},
	}
	for _, c := range cases {
		o, _, smiCalls := testOpeners(c.pdhErr, c.smiErr)
		log, buf := captureLog()
		p, err := detect(context.Background(), "auto", log, o)
		if err != nil || p.Name() != c.want {
			t.Fatalf("%s: %v %v", c.name, p, err)
		}
		out := buf.String()
		if n := strings.Count(out, "level=WARN"); n != c.warns {
			t.Errorf("%s: %d warnings, want %d:\n%s", c.name, n, c.warns, out)
		}
		for _, r := range c.reasons {
			if !strings.Contains(out, r) {
				t.Errorf("%s: reason %q not logged:\n%s", c.name, r, out)
			}
		}
		if !strings.Contains(out, "vram source selected") || !strings.Contains(out, "source="+c.want) {
			t.Errorf("%s: selection not logged:\n%s", c.name, out)
		}
		if c.want == "pdh" && *smiCalls != 0 {
			t.Errorf("%s: nvidia-smi probed although pdh works", c.name)
		}
	}
	if p, err := detect(context.Background(), "", nil, func() openers { o, _, _ := testOpeners(nil, nil); return o }()); err != nil || p.Name() != "pdh" {
		t.Fatalf("empty source must behave as auto: %v %v", p, err)
	}
}

func TestDetectExplicitSources(t *testing.T) {
	down := errors.New("down")
	o, _, _ := testOpeners(down, down)
	for _, src := range []string{"pdh", "nvidia-smi"} {
		if _, err := detect(context.Background(), src, nil, o); !errors.Is(err, down) || !strings.Contains(err.Error(), src) {
			t.Errorf("%s: want fatal error naming the source, got %v", src, err)
		}
	}
	ok, pdhCalls, smiCalls := testOpeners(nil, nil)
	if p, err := detect(context.Background(), "nvidia-smi", nil, ok); err != nil || p.Name() != "nvidia-smi" || *pdhCalls != 0 {
		t.Fatalf("nvidia-smi: %v %v pdhCalls=%d", p, err, *pdhCalls)
	}
	if p, err := detect(context.Background(), "pdh", nil, ok); err != nil || p.Name() != "pdh" || *smiCalls != 1 {
		t.Fatalf("pdh: %v %v smiCalls=%d", p, err, *smiCalls)
	}
	log, buf := captureLog()
	if p, err := detect(context.Background(), "none", log, ok); err != nil || p.Name() != "none" || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("none: %v %v\n%s", p, err, buf)
	}
	if _, err := detect(context.Background(), "nvml", nil, ok); err == nil {
		t.Fatal("unknown source must be an error")
	}
}
