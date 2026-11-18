// Package i18n — язык трея и окон хоста: русский, английский и китайский. Выбор человека хранится в маленьком файле
// (DefaultPath); без файла язык берётся из языка интерфейса Windows (Detect).
package i18n

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Lang — код языка: "ru", "en" или "zh".
type Lang string

const (
	RU Lang = "ru"
	EN Lang = "en"
	ZH Lang = "zh"
)

// All — языки в порядке подменю «Язык».
var All = []Lang{RU, EN, ZH}

// Parse — код языка из строки: регистр и пробелы по краям не важны; региональная часть ("zh-CN", "en_US")
// отбрасывается. Другой язык — false.
func Parse(s string) (Lang, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "-_"); i >= 0 {
		s = s[:i]
	}
	for _, l := range All {
		if s == string(l) {
			return l, true
		}
	}
	return "", false
}

// Native — название языка на нём самом (пункт подменю «Язык»).
func (l Lang) Native() string {
	switch l {
	case EN:
		return "English"
	case ZH:
		return "中文"
	}
	return "Русский"
}

var current atomic.Value // Lang

// Current — язык трея и окон; до Set — русский.
func Current() Lang {
	if l, ok := current.Load().(Lang); ok {
		return l
	}
	return RU
}

// Set меняет язык трея и окон.
func Set(l Lang) { current.Store(l) }

// T — текст ключа k на языке l; неизвестный язык — русский.
func (l Lang) T(k Key) string {
	if k < 0 || k >= keyCount {
		return ""
	}
	switch l {
	case EN:
		return en[k]
	case ZH:
		return zh[k]
	}
	return ru[k]
}

// F — текст ключа k, подставленный через fmt.Sprintf.
func (l Lang) F(k Key, a ...any) string { return fmt.Sprintf(l.T(k), a...) }

// Num1 — число с одним знаком после запятой: у английского и китайского — точка, у русского — запятая.
func (l Lang) Num1(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	if l != EN && l != ZH {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// DefaultPath — файл выбранного языка: %LOCALAPPDATA%\locallm\lang. Не зависит от конфига: язык нужен и окну
// второго экземпляра, до чтения конфига. Нет LOCALAPPDATA — "" (язык не сохраняется).
func DefaultPath() string {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "locallm", "lang")
}

// Load читает язык из файла path; нет файла, он не читается или в нём не код языка — false.
func Load(path string) (Lang, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64 {
		return "", false
	}
	return Parse(string(data))
}

// Save записывает код языка в файл path; каталог создаётся при необходимости.
func Save(path string, l Lang) error {
	if path == "" {
		return errors.New("language file path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("language file %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(string(l)+"\n"), 0o600); err != nil {
		return fmt.Errorf("language file %s: %w", path, err)
	}
	return nil
}

// Startup — язык при старте хоста: из файла path, иначе Detect.
func Startup(path string) Lang {
	if l, ok := Load(path); ok {
		return l
	}
	return Detect()
}

// fromLangID — язык по LANGID Windows: основной язык 0x19 — русский, 0x04 — китайский, остальные — английский.
func fromLangID(id uint16) Lang {
	switch id & 0x3ff {
	case 0x19:
		return RU
	case 0x04:
		return ZH
	}
	return EN
}
