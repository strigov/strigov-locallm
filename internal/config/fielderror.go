package config

// FieldError — нарушение правила с полем документа (§4.4 спецификации v2). Field — путь в документе:
// "models.qwen.args[3]" для файла, "models[2].profiles[0].args[3]" для документа API; пусто — ошибка без поля.
// Message не содержит значений аргументов (§5.5 п. 14).
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}
