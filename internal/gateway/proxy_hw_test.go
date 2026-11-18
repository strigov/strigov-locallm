//go:build hw && windows

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/hwtest"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/supervisor"
)

const s5Body = `{"model":"tiny","stream":%t,"max_tokens":%d,"ignore_eos":true,"temperature":0,"seed":1,` +
	`"messages":[{"role":"user","content":"Count from 1 to 1000, one number per line."}]}`

func startSmall(t *testing.T) *scheduler.Upstream {
	t.Helper()
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	s := supervisor.New(hwtest.Config(t, exe, 8081), nil)
	up, err := s.Start(context.Background(), config.Model{ID: "tiny", Path: model, Args: []string{"-c", "8192"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Retire(scheduler.RetireStop); _ = s.Stop(context.Background()) })
	return up
}

func proxyFor(t *testing.T, up *scheduler.Upstream) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveProxy(w, r, up) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// stream возвращает время прихода каждого SSE-события от начала запроса.
func stream(t *testing.T, base string, tokens int) []time.Duration {
	t.Helper()
	start := time.Now()
	at := streamTimes(t, base, tokens)
	out := make([]time.Duration, len(at))
	for i := range at {
		out[i] = at[i].Sub(start)
	}
	return out
}

func streamTimes(t *testing.T, base string, tokens int) []time.Time {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(fmt.Sprintf(s5Body, true, tokens)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var at []time.Time
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			at = append(at, time.Now())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return at
}

func quantile(d []time.Duration, q float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(float64(len(s)-1)*q)]
}

func intervals(at []time.Time) []time.Duration {
	out := make([]time.Duration, 0, len(at))
	for i := 1; i < len(at); i++ {
		out = append(out, at[i].Sub(at[i-1]))
	}
	return out
}

// s5TapBody timestamps data lines on the upstream side without changing response bytes.
// Both sides use time.Now's monotonic clock in this test process.
type s5TapBody struct {
	io.ReadCloser
	mu   sync.Mutex
	line []byte
	at   []time.Time
}

func (b *s5TapBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	at := time.Now()
	b.mu.Lock()
	for _, c := range p[:n] {
		if c == '\n' {
			if bytes.HasPrefix(b.line, []byte("data: ")) {
				b.at = append(b.at, at)
			}
			b.line = b.line[:0]
		} else {
			b.line = append(b.line, c)
		}
	}
	b.mu.Unlock()
	return n, err
}

func (b *s5TapBody) times() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.at)
}

// s5TappedProxy inserts a test-only tap between serveProxy and llama-server.
// wrap is used only by the buffering controls; the hardware test passes nil.
func s5TappedProxy(t *testing.T, up *scheduler.Upstream, wrap func(http.Handler) http.Handler) (string, <-chan *s5TapBody) {
	t.Helper()
	taps := make(chan *s5TapBody, 1)
	tapServer := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(up.URL) },
		Transport:     up.Transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			body := &s5TapBody{ReadCloser: resp.Body}
			resp.Body = body
			taps <- body
			return nil
		},
	})
	t.Cleanup(tapServer.Close)
	u, err := url.Parse(tapServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	tapped := scheduler.NewUpstream(up.Model, u, up.PID, nil)
	t.Cleanup(func() { tapped.Retire(scheduler.RetireStop) })
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveProxy(w, r, tapped) })
	if wrap != nil {
		handler = wrap(handler)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	return proxy.URL, taps
}

func s5AddedLatency(t *testing.T, upstream, client []time.Time) []time.Duration {
	t.Helper()
	if len(upstream) != len(client) || len(upstream) < 200 {
		t.Fatalf("%d events upstream, %d at client; want matching counts >= 200", len(upstream), len(client))
	}
	added := make([]time.Duration, len(client))
	for i := range client {
		added[i] = client[i].Sub(upstream[i])
		if added[i] < 0 {
			t.Fatalf("event %d arrived before its upstream timestamp", i)
		}
	}
	return added
}

func checkS5Streaming(added, upstreamIntervals, clientIntervals []time.Duration) error {
	if med := quantile(added, 0.5); med > 5*time.Millisecond {
		return fmt.Errorf("median added latency %s > 5ms", med)
	}
	upstreamMedian, clientMedian := quantile(upstreamIntervals, 0.5), quantile(clientIntervals, 0.5)
	if upstreamMedian <= 0 {
		return fmt.Errorf("upstream event gaps have zero median; streaming measurement is inconclusive")
	}
	if clientMedian < upstreamMedian/2 {
		return fmt.Errorf("client event gaps collapsed: median %s < half of upstream %s", clientMedian, upstreamMedian)
	}
	return nil
}

func TestS5StreamingNotBuffered(t *testing.T) {
	up := startSmall(t)
	proxied, taps := s5TappedProxy(t, up, nil)
	stream(t, up.URL.String(), 16) // прогрев
	stream(t, proxied, 16)
	<-taps // discard the warm-up observation
	var added, uInt, pInt []time.Duration
	for run := 1; run <= 3; run++ {
		p := streamTimes(t, proxied, 200)
		u := (<-taps).times()
		added = append(added, s5AddedLatency(t, u, p)...)
		uInt, pInt = append(uInt, intervals(u)...), append(pInt, intervals(p)...)
		t.Logf("S5 run %d: events upstream=%d client=%d; first added=%s; upstream span=%s client span=%s",
			run, len(u), len(p), p[0].Sub(u[0]), u[len(u)-1].Sub(u[0]), p[len(p)-1].Sub(p[0]))
	}
	t.Logf("S5: same-request events=%d; median added latency per event %s; added p90=%s max=%s; intervals upstream p50=%s p90=%s; client p50=%s p90=%s",
		len(added), quantile(added, 0.5), quantile(added, 0.9), slices.Max(added), quantile(uInt, 0.5), quantile(uInt, 0.9), quantile(pInt, 0.5), quantile(pInt, 0.9))
	if err := checkS5Streaming(added, uInt, pInt); err != nil {
		t.Error(err)
	}
}

type s5BufferedWriter struct {
	http.ResponseWriter
	out io.Writer
}

func (w *s5BufferedWriter) Write(p []byte) (int, error) { return w.out.Write(p) }
func (w *s5BufferedWriter) Flush()                      {} // deliberately withhold flushes

// Exercise the actual tap and checker with known buffering faults, without a GPU.
func TestS5MeasurementRejectsBuffering(t *testing.T) {
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 200; i++ {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 256)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	for _, mode := range []string{"unbuffered", "whole response", "4 KiB"} {
		t.Run(mode, func(t *testing.T) {
			var wrap func(http.Handler) http.Handler
			if mode != "unbuffered" {
				wrap = func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if mode == "whole response" {
							var body bytes.Buffer
							next.ServeHTTP(&s5BufferedWriter{w, &body}, r)
							_, _ = io.Copy(w, &body)
						} else {
							body := bufio.NewWriterSize(w, 4096)
							next.ServeHTTP(&s5BufferedWriter{w, body}, r)
							_ = body.Flush()
						}
					})
				}
			}
			proxy, taps := s5TappedProxy(t, up, wrap)
			p := streamTimes(t, proxy, 200)
			u := (<-taps).times()
			err := checkS5Streaming(s5AddedLatency(t, u, p), intervals(u), intervals(p))
			if mode == "unbuffered" && err != nil {
				t.Fatal(err)
			}
			if mode != "unbuffered" && err == nil {
				t.Fatal("buffered stream passed the S5 measurement")
			}
			t.Logf("S5 control %s: %v", mode, err)
		})
	}
}

func firstToken(t *testing.T, base string) time.Duration {
	t.Helper()
	at := stream(t, base, 8)
	if len(at) == 0 {
		t.Fatal("no events")
	}
	return at[0]
}

// TestS5ClientAbortStopsGeneration — при -np 1 короткий запрос после обрыва длинного получает первый
// токен не позже обычного + 1 с только если прерванная генерация остановлена.
func TestS5ClientAbortStopsGeneration(t *testing.T) {
	up := startSmall(t)
	px := proxyFor(t, up)
	firstToken(t, px) // прогрев
	normal := quantile([]time.Duration{firstToken(t, px), firstToken(t, px), firstToken(t, px)}, 0.5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, px+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(s5Body, true, 4000)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for n := 0; n < 20; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			n++
		}
	}
	cancel()
	resp.Body.Close()
	afterStream := firstToken(t, px)

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx2, http.MethodPost, px+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(s5Body, false, 4000)))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(2 * time.Second)
	select {
	case <-done:
		t.Fatal("long non-streaming request ended before cancellation")
	default:
	}
	cancel2()
	<-done
	afterNonStream := firstToken(t, px)

	t.Logf("S5: first token normal=%s, after stream abort=%s, after non-stream abort=%s (limit normal + 1s)", normal, afterStream, afterNonStream)
	if afterStream > normal+time.Second {
		t.Errorf("stream abort did not stop generation: %s > %s + 1s", afterStream, normal)
	}
	if afterNonStream > normal+time.Second {
		t.Errorf("non-stream abort did not stop generation: %s > %s + 1s", afterNonStream, normal)
	}
}
