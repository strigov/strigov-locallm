package config

import "sync/atomic"

// Live — текущий конфиг для шлюза, контроллера и трея (§4.5, §5.9 спецификации v2). Глобальные поля не меняются всё
// время жизни процесса; Store подменяет конфиг целиком (новый каталог после «Сохранить», «Применить файл», переноса).
type Live struct {
	p atomic.Pointer[Config]
}

func NewLive(c *Config) *Live {
	l := &Live{}
	l.p.Store(c)
	return l
}

func (l *Live) Load() *Config { return l.p.Load() }

func (l *Live) Store(c *Config) { l.p.Store(c) }
