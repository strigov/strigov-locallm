package control

import (
	"errors"
	"fmt"

	"github.com/strigov/strigov-locallm/internal/config"
)

// Ошибки сохранения моделей (§9.6 спецификации v2). Тексты — те, что уходят клиенту.
var (
	ErrNotMigrated     = errors.New("models are defined in locallm.yaml; migrate them to models.yaml first")
	ErrIfMatchRequired = errors.New("If-Match header with the ETag from GET /api/admin/models is required")
)

// InvalidError — документ не прошёл проверку (422 config_invalid); Details без значений аргументов.
type InvalidError struct{ Details []config.FieldError }

func (e *InvalidError) Error() string {
	return fmt.Sprintf("models document is invalid: %d problems", len(e.Details))
}

// DefaultError — PUT удаляет или переименовывает swap.default_model (409 model_is_default).
type DefaultError struct{ Model string }

func (e *DefaultError) Error() string {
	return fmt.Sprintf("model %q is swap.default_model in locallm.yaml; change it there first", e.Model)
}

// WriteError — файл не прочитан или не записан, перенос не удался (500 config_write_failed); каталог автомата
// возвращён откатом. Ошибка проверки переноса — ровно текст спеки "migration check failed" (§5.8 п. 4).
type WriteError struct{ Err error }

func (e *WriteError) Error() string {
	if errors.Is(e.Err, config.ErrMigrationCheck) {
		return config.ErrMigrationCheck.Error()
	}
	return "cannot write models.yaml: " + e.Err.Error()
}
func (e *WriteError) Unwrap() error { return e.Err }
