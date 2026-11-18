package config

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	t.Setenv("LOCALLM_X", `C:\x`)
	t.Setenv("LOCALLM_EMPTY", "")
	t.Setenv("LOCALLM_LITERAL", "%LOCALLM_X%")
	cases := []struct{ in, want, err string }{
		{`%LOCALLM_X%\a.gguf`, `C:\x\a.gguf`, ""},
		{`100%%`, `100%`, ""},
		{`a%%b%LOCALLM_X%`, `a%bC:\x`, ""},
		{`plain`, `plain`, ""},
		{``, ``, ""},
		{`%LOCALLM_X%%LOCALLM_X%`, `C:\xC:\x`, ""},
		{`%LOCALLM_EMPTY%`, ``, ""},
		{`%LOCALLM_LITERAL%`, `%LOCALLM_X%`, ""},
		{`%NO_SUCH_VAR_LOCALLM%`, "", "unknown environment variable %NO_SUCH_VAR_LOCALLM%"},
		{`50%`, "", "unterminated %"},
	}
	for _, c := range cases {
		got, err := expand(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("expand(%q) err=%v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("expand(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
