package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// modelsHeader — первые две строки models.yaml, который пишет панель (пример §5.3 спецификации v2).
const modelsHeader = "# models.yaml — модели locallm-host. Файл перезаписывает веб-панель, комментарии при этом не сохраняются.\n" +
	"# После ручной правки: панель → «Применить файл» или перезапуск хоста.\n"

var plainNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// EncodeModels — детерминированный текст models.yaml (§5.7): заголовок, version: 1, models (пустой каталог —
// models: {}); у модели path, затем args (если не пуст), затем profiles (если есть); списки — в одну строку.
// ParseModels(EncodeModels(d)) возвращает d (пустые списки — nil).
func EncodeModels(doc *ModelsDoc) []byte {
	var b strings.Builder
	b.WriteString(modelsHeader)
	b.WriteString("version: 1\n")
	if doc == nil || len(doc.Models) == 0 {
		b.WriteString("models: {}\n")
		return []byte(b.String())
	}
	b.WriteString("models:\n")
	for _, m := range doc.Models {
		fmt.Fprintf(&b, "  %s:\n    path: %s\n", encodeName(m.ID), encodeString(m.Path))
		if len(m.Args) > 0 {
			fmt.Fprintf(&b, "    args: %s\n", encodeList(m.Args))
		}
		if len(m.Profiles) > 0 {
			b.WriteString("    profiles:\n")
			for _, p := range m.Profiles {
				fmt.Fprintf(&b, "      %s: %s\n", encodeName(p.Name), encodeList(p.Args))
			}
		}
	}
	return []byte(b.String())
}

// encodeName — имя модели или профиля: без кавычек, если yaml.v3 прочитает его как строку (тег !!str), иначе в кавычках
// ('1.5', 'true').
func encodeName(s string) string {
	if plainNameRe.MatchString(s) && (&yaml.Node{Kind: yaml.ScalarNode, Value: s}).ShortTag() == "!!str" {
		return s
	}
	return encodeString(s)
}

// encodeString — строка в одинарных кавычках (' удваивается); строка, которую YAML в одинарных кавычках не сохранит
// (бывает только в ручных значениях, PV8), — в двойных кавычках с экранированием.
func encodeString(s string) string {
	if !needsEscape(s) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02X`, r)
		case needsEscapeRune(r):
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// needsEscape — не UTF-8 (в двойных кавычках неверные байты станут U+FFFD) или есть символ, который yaml.v3 не примет
// как есть (управляющие, U+007F) или свернёт как перевод строки.
func needsEscape(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || needsEscapeRune(r) {
			return true
		}
	}
	return false
}

// needsEscapeRune — C1 (U+0080–U+009F, в том числе NEL), U+2028, U+2029, BOM U+FEFF, не-символы U+FFFE и U+FFFF.
func needsEscapeRune(r rune) bool {
	return (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 || r == 0xfeff || r == 0xfffe || r == 0xffff
}

func encodeList(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		q[i] = encodeString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
