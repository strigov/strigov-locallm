package tray

import (
	"bytes"
	_ "embed"
	"image/color"
)

// icoSizes — размеры картинок внутри ICO (§11.1).
var icoSizes = []int{16, 20, 24, 32, 48}

// Готовый эмодзи 🦙 с цветной точкой состояния. Все размеры встроены в exe.
// Источник и способ обновления — assets/README.md.
//
//go:embed assets/gray.ico
var grayICO []byte

//go:embed assets/yellow.ico
var yellowICO []byte

//go:embed assets/green.ico
var greenICO []byte

//go:embed assets/blue.ico
var blueICO []byte

// rgba — цвет точки состояния; палитра совпадает с лампой в веб-панели.
func (c Color) rgba() color.NRGBA {
	switch c {
	case Yellow:
		return color.NRGBA{R: 0xf2, G: 0xc0, B: 0x1e, A: 0xff}
	case Green:
		return color.NRGBA{R: 0x2e, G: 0xa0, B: 0x43, A: 0xff}
	case Blue:
		return color.NRGBA{R: 0x1f, G: 0x6f, B: 0xeb, A: 0xff}
	default:
		return color.NRGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff}
	}
}

// ICO возвращает значок 🦙 с индикатором цвета c (PNG внутри, Windows Vista+).
func ICO(c Color) []byte {
	switch c {
	case Yellow:
		return bytes.Clone(yellowICO)
	case Green:
		return bytes.Clone(greenICO)
	case Blue:
		return bytes.Clone(blueICO)
	default:
		return bytes.Clone(grayICO)
	}
}
