// Package fakeupstream — фейковый llama-server для спайков S6/S7 и тестов без GPU (§14.3).
package fakeupstream

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// loadingBody — тело 503 llama-server во время загрузки (§7.3), байт в байт.
const loadingBody = `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`

type Options struct {
	ModelID      string
	Loading      time.Duration
	FailLoad     bool
	CrashAfter   time.Duration
	DelayHeaders time.Duration
	SSEInterval  time.Duration
	Chunks       int
	Status       int
	RetryAfter   int
	ShouldRetry  string
}

type Config struct {
	Listen  string
	Options Options
}

// ParseFlags разбирает флаги §14.3. Неизвестный флаг — ошибка.
func ParseFlags(args []string, errOut io.Writer) (Config, error) {
	fs := flag.NewFlagSet("fakeupstream", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var c Config
	var model, host string
	var port int
	o := &c.Options
	fs.StringVar(&c.Listen, "listen", "127.0.0.1:8080", "listen address host:port")
	fs.StringVar(&model, "m", "", "model file; its base name without extension becomes the model id (file need not exist)")
	fs.StringVar(&host, "host", "", "overrides the host part of --listen")
	fs.IntVar(&port, "port", 0, "overrides the port part of --listen")
	fs.DurationVar(&o.Loading, "loading", 0, "answer 503 Loading model on all paths for this long")
	fs.BoolVar(&o.FailLoad, "fail-load", false, "after --loading print 5 error lines to stderr and exit 1")
	fs.DurationVar(&o.CrashAfter, "crash-after", 0, "exit with code 3 this long after ready (0 = never)")
	fs.DurationVar(&o.DelayHeaders, "delay-headers", 0, "pause before response headers on POST /v1/*")
	fs.DurationVar(&o.SSEInterval, "sse-interval", 0, "pause between SSE chunks for stream=true")
	fs.IntVar(&o.Chunks, "chunks", 20, "number of content chunks")
	fs.IntVar(&o.Status, "status", 200, "answer POST /v1/* with this status and an OpenAI error body")
	fs.IntVar(&o.RetryAfter, "retry-after", 0, "add Retry-After: N to the --status response")
	fs.StringVar(&o.ShouldRetry, "should-retry", "", "add x-should-retry: <value> to the --status response")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	validationError := func(err error) (Config, error) {
		fmt.Fprintln(fs.Output(), "fakeupstream:", err)
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return validationError(fmt.Errorf("unexpected arguments: %q", fs.Args()))
	}
	h, p, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return validationError(fmt.Errorf("--listen: %w", err))
	}
	if host != "" {
		h = host
	}
	if port != 0 {
		p = strconv.Itoa(port)
	}
	c.Listen = net.JoinHostPort(h, p)
	o.ModelID = "fake"
	if model != "" {
		base := filepath.Base(model)
		o.ModelID = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if o.Chunks < 1 {
		return validationError(errors.New("--chunks must be >= 1"))
	}
	if o.Status < 100 || o.Status > 599 {
		return validationError(errors.New("--status must be in 100..599"))
	}
	return c, nil
}

// Server — http.Handler фейкового llama-server. Отсчёт --loading идёт от New.
type Server struct {
	o     Options
	start time.Time
	logMu sync.Mutex
	log   io.Writer
}

func New(o Options, log io.Writer) *Server {
	if o.ModelID == "" {
		o.ModelID = "fake"
	}
	if o.Chunks < 1 {
		o.Chunks = 20
	}
	if o.Status == 0 {
		o.Status = http.StatusOK
	}
	if log == nil {
		log = io.Discard
	}
	return &Server{o: o, start: time.Now(), log: log}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	begin := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	s.route(rec, r)
	s.logMu.Lock()
	fmt.Fprintf(s.log, "%s %s %s %d %dms aborted=%t\n", begin.UTC().Format(time.RFC3339Nano), r.Method, r.URL.Path,
		rec.status, time.Since(begin).Milliseconds(), r.Context().Err() != nil)
	s.logMu.Unlock()
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if time.Since(s.start) < s.o.Loading {
		writeRaw(w, http.StatusServiceUnavailable, loadingBody)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeRaw(w, http.StatusOK, `{"status":"ok"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": s.o.ModelID, "object": "model", "owned_by": "fakeupstream", "created": s.start.Unix()},
		}})
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions"):
		s.completion(w, r)
	default:
		writeJSON(w, http.StatusNotFound, errorBody(http.StatusNotFound, "not found: "+r.Method+" "+r.URL.Path))
	}
}

func (s *Server) completion(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if !sleepCtx(r.Context(), s.o.DelayHeaders) {
		return
	}
	if s.o.Status != http.StatusOK {
		if s.o.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s.o.RetryAfter))
		}
		if s.o.ShouldRetry != "" {
			w.Header().Set("x-should-retry", s.o.ShouldRetry)
		}
		writeJSON(w, s.o.Status, errorBody(s.o.Status, fmt.Sprintf("fakeupstream: forced status %d", s.o.Status)))
		return
	}
	chat := r.URL.Path == "/v1/chat/completions"
	if !req.Stream {
		writeJSON(w, http.StatusOK, s.full(chat))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush(w)
	for i := 1; i <= s.o.Chunks; i++ {
		if i > 1 && !sleepCtx(r.Context(), s.o.SSEInterval) {
			return
		}
		b, _ := json.Marshal(s.chunk(chat, i))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		flush(w)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush(w)
}

func (s *Server) text() string {
	parts := make([]string, s.o.Chunks)
	for i := range parts {
		parts[i] = fmt.Sprintf("tok%d", i+1)
	}
	return strings.Join(parts, " ")
}

func (s *Server) full(chat bool) map[string]any {
	usage := map[string]any{"prompt_tokens": 1, "completion_tokens": s.o.Chunks, "total_tokens": s.o.Chunks + 1}
	if chat {
		return map[string]any{"id": "fake-1", "object": "chat.completion", "created": time.Now().Unix(), "model": s.o.ModelID,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": s.text()}, "finish_reason": "stop"}},
			"usage":   usage}
	}
	return map[string]any{"id": "fake-1", "object": "text_completion", "created": time.Now().Unix(), "model": s.o.ModelID,
		"choices": []any{map[string]any{"index": 0, "text": s.text(), "finish_reason": "stop"}}, "usage": usage}
}

func (s *Server) chunk(chat bool, i int) map[string]any {
	tok := fmt.Sprintf("tok%d ", i)
	if chat {
		return map[string]any{"id": "fake-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.o.ModelID,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": tok}, "finish_reason": nil}}}
	}
	return map[string]any{"id": "fake-1", "object": "text_completion", "created": time.Now().Unix(), "model": s.o.ModelID,
		"choices": []any{map[string]any{"index": 0, "text": tok, "finish_reason": nil}}}
}

// Run слушает c.Listen и обслуживает запросы до отмены ctx. --fail-load и --crash-after вызывают exit.
func Run(ctx context.Context, c Config, stdout, stderr io.Writer, exit func(int)) error {
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	o := c.Options
	srv := &http.Server{Handler: New(o, stdout), ReadHeaderTimeout: 10 * time.Second}
	switch {
	case o.FailLoad:
		t := time.AfterFunc(o.Loading, func() {
			for i := 1; i <= 5; i++ {
				fmt.Fprintf(stderr, "fakeupstream: simulated load failure, line %d of 5\n", i)
			}
			exit(1)
		})
		defer t.Stop()
	case o.CrashAfter > 0:
		t := time.AfterFunc(o.Loading+o.CrashAfter, func() {
			fmt.Fprintln(stderr, "fakeupstream: simulated crash")
			exit(3)
		})
		defer t.Stop()
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Fprintf(stderr, "fakeupstream: listening on %s model=%s\n", ln.Addr(), o.ModelID)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() { flush(r.ResponseWriter) }

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func errorBody(status int, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg, "type": "fake_error", "code": status}}
}

func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeRaw(w, status, string(b))
}
