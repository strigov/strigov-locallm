# Эталоны router mode (P0-REF, спецификация §14.2)

- Сборка: `version: 0.6.0-dev (build 11476, commit 988190680)` (`bin\router-version.txt`).
- Дата съёмки: 2026-11-05 (01:56 +07:00), однократный захват оркестратором вне sandbox; повторный захват не выполнялся, 13 JSON не перезаписывать.
- Команда: `llama-server.exe --models-preset testdata\routermode\presets.ini --models-max 1 --host 127.0.0.1 --port 8090`. При захвате `LLAMA_API_KEY` и все `LLAMA_ARG_*` были обнулены в окружении текущего процесса PowerShell через `SetEnvironmentVariable(..., $null, 'Process')`, а не удалены: pwsh 7.6 / .NET 10 оставляет пустые переменные. Значение ключа дочернему процессу не передалось; `bin\router.err.log:12` и `bin\router.out.log:3,30,58` сообщают `no API key is set`. Значения не читались и не журналировались. Для будущих запусков блоки плана используют `Remove-Item -LiteralPath "Env:$_"`; повторный захват не требуется и не выполняется.
- Модель «неудачи»: `big-oom` (ctx-size 262144) НЕ дала неудачу — дошла до `loaded`; сработала `broken` (несуществующий файл), `exit_code` 1. Эталон `*_failed.json` — это неудача «файл не найден», а не нехватка памяти (OOM не снимался).
- Отклонения от пресета: нет (router принял `presets.ini` без правок; `--help` не потребовался; секция `[broken]` принята).
- Формат: `models_*.json`, `v1_models_*.json` — тело ответа как есть; `load_*.json`, `unload_*.json` — `{"status": <код>, "body": <тело>}`.
- `/v1/models` в этой сборке совпадает с `/models` побайтно, поля `context_length` в нём нет. `context_length` — наше расширение по §9.3, допустимое в `/v1/models` хоста. Контрактный тест шлюза (план 2) сравнивает структуру: ключи и типы значений; значения не сравниваются. Список содержит 3 записи из кэша `llama-server` этого ПК (`source` = `cache`); структура у них та же.
- Неудача в `/models*`: `status.value` остаётся `unloaded`, добавляются `status.failed: true` и `status.exit_code: 1`; у остальных записей этих полей нет.
- Реальные состояния проверены по телам: `loading` — `qwen-coder` со `status.value = "loading"` (первая в списке `big-oom` — `unloaded`, это другая модель); `loaded` — `qwen-coder` со `status.value = "loaded"` и `meta`.
- `pi_required_fields.json` составлен по `client.ts` pi на SHA `707888ece685778881c40d232880c5a0b93a12f1` (прочитан как данные, не исполнялся), `provisional: false`.

| Файл | Что снято | provisional |
|---|---|---|
| models_unloaded.json | GET /models, ничего не загружено | false |
| models_loading.json | GET /models сразу после POST /models/load qwen-coder | false |
| models_loaded.json | GET /models, qwen-coder загружена | false |
| models_failed.json | GET /models после неудачной загрузки `broken` (файл не найден, не OOM) | false |
| v1_models_unloaded.json | GET /v1/models, ничего не загружено | false |
| v1_models_loading.json | GET /v1/models во время загрузки | false |
| v1_models_loaded.json | GET /v1/models, qwen-coder загружена | false |
| v1_models_failed.json | GET /v1/models после неудачной загрузки `broken` (файл не найден, не OOM) | false |
| load_ok.json | POST /models/load qwen-coder | false |
| load_already_loaded.json | POST /models/load qwen-coder повторно | false |
| load_unknown.json | POST /models/load no-such-model | false |
| unload_ok.json | POST /models/unload qwen-coder | false |
| unload_not_loaded.json | POST /models/unload tiny (не загружена) | false |
| pi_required_fields.json | обязательные поля pi из client.ts | false |
| pi_sequence.log | журнал запросов pi через logproxy (п. 4, владелец) | снят 2026-11-07, не хранится |
