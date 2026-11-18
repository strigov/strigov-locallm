package vram

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestParseSMI(t *testing.T) {
	ok := map[string]int64{"1234\r\n": 1234, "100\n200\n": 300, " 512 \r\n\r\n": 512, "0": 0}
	for in, want := range ok {
		if got, err := parseSMI([]byte(in)); err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "\r\n", "[N/A]\r\n", "abc", "100\nfoo\n", "-5"} {
		if _, err := parseSMI([]byte(in)); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestNvidiaSMIRunsExactCommandWithTimeout(t *testing.T) {
	var name string
	var args []string
	var left time.Duration
	p := newNvidiaSMI(func(ctx context.Context, n string, a ...string) ([]byte, error) {
		name, args = n, a
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("nvidia-smi must run with a deadline")
		}
		left = time.Until(d)
		return []byte("2048\r\n"), nil
	})
	got, err := p.UsageMiB(context.Background())
	if err != nil || got != 2048 {
		t.Fatalf("got %d, %v", got, err)
	}
	if name != "nvidia-smi" || !reflect.DeepEqual(args, []string{"--query-gpu=memory.used", "--format=csv,noheader,nounits"}) {
		t.Fatalf("command %s %v", name, args)
	}
	if left <= 0 || left > 3*time.Second {
		t.Fatalf("deadline in %s, want <= 3s", left)
	}
	if p.Name() != "nvidia-smi" {
		t.Fatal(p.Name())
	}
}

func TestNvidiaSMIRunErrorIsWrapped(t *testing.T) {
	boom := errors.New("exit status 9")
	p := newNvidiaSMI(func(context.Context, string, ...string) ([]byte, error) { return nil, boom })
	if _, err := p.UsageMiB(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

func TestNvidiaSMIRespectsEarlierCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	p := newNvidiaSMI(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
			t.Fatalf("deadline %s, %v; want %s", got, ok, want)
		}
		return []byte("0"), nil
	})
	if _, err := p.UsageMiB(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNvidiaSMIPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := newNvidiaSMI(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, ctx.Err()
	})
	if _, err := p.UsageMiB(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestNoneProbe(t *testing.T) {
	var p Probe = none{}
	if _, err := p.UsageMiB(context.Background()); !errors.Is(err, ErrUnavailable) || p.Name() != "none" {
		t.Fatalf("none: %v %s", err, p.Name())
	}
}
