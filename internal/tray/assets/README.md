# Иконка трея

`llama.png` — эмодзи 🦙 «Llama» в стиле 3D из набора Microsoft
[Fluent Emoji](https://github.com/microsoft/fluentui-emoji)
(`assets/Llama/3D/llama_3d.png`, коммит `1ffb34c752ecf5d402f04cfb4b392c77f57c54bc`).
Лицензия MIT, © Microsoft Corporation; текст — `LICENSE-fluentui-emoji.txt` рядом.

Четыре ICO добавляют к ламе тонкий тёмный контур (чтобы светлая лама не терялась на светлой
панели задач) и цветную точку состояния внизу справа; сама лама не меняет цвет.
Каждый ICO содержит PNG в размерах 16, 20, 24, 32 и 48 пикселей и встроен в exe через
`go:embed`. При запуске не нужны шрифты, Python или дополнительные файлы.

Для обновления ICO из `llama.png`: `python scripts/generate-tray-icons.py`.
Для генерации нужен Pillow; при обычной сборке он не используется.
