package gateway

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Ошибки /api/admin/* (§9.6 спецификации v2). Формат и запись — apierror.go.

func errAdminUnauthorized() *apiError {
	return &apiError{Status: http.StatusUnauthorized, Code: "invalid_admin_credentials", Type: "authentication_error",
		Message: control.ErrBadCredentials.Error(), Headers: map[string]string{"WWW-Authenticate": "Bearer"}}
}

func errTooManyAttempts() *apiError {
	return &apiError{Status: http.StatusTooManyRequests, Code: "too_many_attempts", Type: "rate_limit_error",
		Message: control.ErrTooManyAttempts.Error(), Headers: map[string]string{"Retry-After": "60"}}
}

// authError — ответ на ошибку входа или Bearer: блокировка §9.4 — 429, иначе 401.
func authError(err error) *apiError {
	if errors.Is(err, control.ErrTooManyAttempts) {
		return errTooManyAttempts()
	}
	return errAdminUnauthorized()
}

// errHostNotAllowed — 403; host — значение Host или Origin, обрезанное clip100.
func errHostNotAllowed(host string) *apiError {
	return &apiError{Status: http.StatusForbidden, Code: "host_not_allowed", Type: "permission_error",
		Message: fmt.Sprintf("host %q is not allowed for the admin interface; add it to server.admin_hosts", host)}
}

func errAdminTooLarge() *apiError {
	return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Type: "invalid_request_error",
		Message: "request body exceeds 1 MiB"}
}

func errUnsupportedMediaType() *apiError {
	return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type", Type: "invalid_request_error",
		Message: "Content-Type must be application/json"}
}

// errAdminBody — 400 тела запроса администратора; деталь не повторяет значений из тела.
func errAdminBody(detail string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Type: "invalid_request_error",
		Message: "invalid request body: " + detail}
}

// adminError — ответ на ошибку контроллера (§9.6 v2). Ошибки автомата (пауза, нет модели, остановка) — как у команд
// MVP (cmdError); model — имя из тела команды, "" — у остальных маршрутов.
func (s *server) adminError(model string, err error) *apiError {
	var iu *scheduler.InUseError
	var def *control.DefaultError
	var ce *config.ChangedError
	var inv *control.InvalidError
	var we *control.WriteError
	conflict := func(code, msg string) *apiError {
		return &apiError{Status: http.StatusConflict, Code: code, Type: "conflict_error", Message: msg}
	}
	switch {
	case errors.As(err, &iu):
		e := conflict("model_in_use", iu.Error())
		e.Extra = map[string]any{"models": iu.Models}
		return e
	case errors.As(err, &def):
		return conflict("model_is_default", def.Error())
	case errors.Is(err, control.ErrNotMigrated):
		return conflict("models_not_migrated", control.ErrNotMigrated.Error())
	case errors.Is(err, config.ErrAlreadyMigrated):
		return conflict("already_migrated", "models are already in models.yaml")
	case errors.As(err, &ce):
		return &apiError{Status: http.StatusPreconditionFailed, Code: "config_changed_on_disk", Type: "conflict_error",
			Message: "models.yaml changed on disk; reload it and repeat", Extra: map[string]any{"etag": ce.ETag}}
	case errors.As(err, &inv):
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "config_invalid", Type: "invalid_request_error",
			Message: inv.Error(), Extra: map[string]any{"details": inv.Details}}
	case errors.Is(err, control.ErrIfMatchRequired):
		return &apiError{Status: http.StatusPreconditionRequired, Code: "if_match_required", Type: "invalid_request_error",
			Message: control.ErrIfMatchRequired.Error()}
	case errors.As(err, &we):
		return &apiError{Status: http.StatusInternalServerError, Code: "config_write_failed", Type: "server_error",
			Message: we.Error()}
	}
	return s.cmdError(model, err)
}
