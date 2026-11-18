package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// expand раскрывает %NAME% значением переменной окружения; %% — литерал %.
// Неизвестная переменная и непарный % — ошибка (§5.3 п. 2).
func expand(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			return "", errors.New("unterminated %")
		}
		name := s[i+1 : i+1+j]
		if name == "" {
			b.WriteByte('%')
		} else {
			v, ok := os.LookupEnv(name)
			if !ok {
				return "", fmt.Errorf("unknown environment variable %%%s%%", name)
			}
			b.WriteString(v)
		}
		i += j + 2
	}
	return b.String(), nil
}

func expandField(errs *[]error, field, s string) string {
	v, err := expand(s)
	if err != nil {
		*errs = append(*errs, FieldError{field, err.Error()})
		return s
	}
	return v
}

func expandArgs(errs *[]error, field string, args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandField(errs, fmt.Sprintf("%s[%d]", field, i), a)
	}
	return out
}
