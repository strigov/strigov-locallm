package gateway

import "github.com/strigov/strigov-locallm/internal/gguf"

// HeaderCache — заголовки GGUF без ожидания диска (§8.3); реализует *gguf.Cache.
type HeaderCache interface {
	Ready(path string) (gguf.Info, bool)
}

// WithHeaders подключает кэш заголовков GGUF: meta.n_ctx_train и meta.n_embd загруженной модели в /models и
// /v1/models (§6.8, §8.3). Без него оба поля — 0, как в MVP.
func WithHeaders(c HeaderCache) Option {
	return func(s *server) { s.headers = c }
}

// headerMeta — ContextTrain и EmbeddingLen файла path из кэша. Кэша нет, путь пуст или записи ещё нет — 0 и 0
// (кэш сам запускает фоновое чтение, обработчик его не ждёт).
func (s *server) headerMeta(path string) (ctxTrain, embd int64) {
	if s.headers == nil || path == "" {
		return 0, 0
	}
	info, ok := s.headers.Ready(path)
	if !ok {
		return 0, 0
	}
	return info.ContextTrain, info.EmbeddingLen
}
