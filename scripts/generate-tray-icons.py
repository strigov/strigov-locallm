"""Обновить ICO значка трея из llama.png: python scripts/generate-tray-icons.py.

Нужен Pillow только для генерации, не для сборки или работы приложения.
Источник llama.png и лицензия — internal/tray/assets/README.md.
"""

from io import BytesIO
from pathlib import Path
import struct

from PIL import Image, ImageDraw, ImageFilter


ASSETS = Path(__file__).resolve().parents[1] / "internal" / "tray" / "assets"
SIZES = (16, 20, 24, 32, 48)
INK = "#243043"   # тёмный контур: светлая лама не теряется на светлой панели задач
CREAM = "#f6f3e9" # светлая кромка точки: точка видна на тёмной панели
COLORS = {
    "gray": "#808080",
    "yellow": "#f2c01e",
    "green": "#2ea043",
    "blue": "#1f6feb",
}


def frame(source, size, color):
    # Отдельный рендер каждого размера в 4-кратном масштабе: края, контур и точка остаются чёткими.
    scale = 4
    big = size * scale
    canvas = Image.new("RGBA", (big, big))
    llama = source.copy()
    llama.thumbnail((big - 3 * scale, big - 3 * scale), Image.Resampling.LANCZOS)
    layer = Image.new("RGBA", (big, big))
    layer.alpha_composite(llama, ((big - llama.width) // 2, (big - llama.height) // 2))

    # Контур толщиной в 1 пиксель итогового размера: расширенная маска силуэта, залитая INK, под ламой.
    mask = layer.getchannel("A").point(lambda a: 255 if a > 96 else 0)
    outline = mask.filter(ImageFilter.MaxFilter(2 * scale + 1))
    canvas.paste(Image.new("RGBA", (big, big), INK), (0, 0), outline)
    canvas.alpha_composite(layer)

    badge = Image.new("RGBA", canvas.size)
    draw = ImageDraw.Draw(badge)
    centre = big * 0.8
    for radius, fill in ((0.195, CREAM), (0.17, INK), (0.135, color)):
        r = big * radius
        draw.ellipse((centre - r, centre - r, centre + r, centre + r), fill=fill)
    canvas = canvas.resize((size, size), Image.Resampling.LANCZOS)
    # Усреднение покрытия сглаживает круг, сохраняя точный цвет в его центре.
    canvas.alpha_composite(badge.resize((size, size), Image.Resampling.BOX))
    output = BytesIO()
    canvas.save(output, format="PNG")
    return output.getvalue()


def main():
    source = Image.open(ASSETS / "llama.png").convert("RGBA")
    source = source.crop(source.getchannel("A").getbbox())  # у исходника Fluent Emoji поля вокруг рисунка
    for name, color in COLORS.items():
        frames = [frame(source, size, color) for size in SIZES]
        output = bytearray(struct.pack("<HHH", 0, 1, len(SIZES)))
        offset = 6 + 16 * len(SIZES)
        for size, data in zip(SIZES, frames):
            output.extend(struct.pack("<BBBBHHII", size, size, 0, 0, 1, 32, len(data), offset))
            offset += len(data)
        for data in frames:
            output.extend(data)
        (ASSETS / f"{name}.ico").write_bytes(output)


if __name__ == "__main__":
    main()
