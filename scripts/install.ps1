# scripts/install.ps1 — установка и обновление locallm-host для текущего пользователя.
# Собирает хост (scripts\build.ps1), кладёт exe в %LOCALAPPDATA%\Programs\locallm, при первой установке
# создаёт конфиг (пустой список моделей), ярлык в «Пуске» и запускает хост с панелью.
# При первой установке спрашивает абсолютные пути к папке llama.cpp и к папке с моделями (если они
# не переданы параметрами). Существующий конфиг не трогает: повторный запуск — это обновление.
param(
  [string]$LlamaDir,  # папка llama.cpp с llama-server.exe (CUDA); можно сразу путь к exe
  [string]$LlamaExe,  # путь к llama-server.exe — прежний параметр, работает как раньше
  [string]$ModelsDir, # папка с файлами .gguf
  [switch]$NoStart    # не запускать хост после установки
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$appDir = Join-Path $env:LOCALAPPDATA 'Programs\locallm'
$exe = Join-Path $appDir 'locallm-host.exe'
$cfgDir = Join-Path $env:LOCALAPPDATA 'locallm'
$cfg = Join-Path $cfgDir 'locallm.yaml'
$models = Join-Path $cfgDir 'models.yaml'
$lnk = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\locallm.lnk'

function Quote([string]$s) { "'" + $s.Replace("'", "''") + "'" } # строка YAML в одинарных кавычках

# Абсолютный локальный путь: C:\… (не C:file, не \\сервер\…, не относительный).
function Test-AbsPath([string]$p) { $p -match '^[A-Za-z]:\\' }

# Убрать кавычки, пробелы и \ на конце (путь, скопированный из Проводника, приходит в кавычках). C:\ остаётся C:\.
function Clean-Path([string]$p) { $p.Trim().Trim('"').Trim("'").Trim().TrimEnd('\') -replace '^([A-Za-z]:)$', '$1\' }

# Спросить абсолютный путь. Пустой ответ — значение по умолчанию. $check возвращает текст проблемы или $null.
function Ask-Path([string]$question, [string]$default, [scriptblock]$check) {
  while ($true) {
    $a = Read-Host "$question`n  [Enter = $default]"
    $p = if ([string]::IsNullOrWhiteSpace($a)) { $default } else { Clean-Path $a }
    if (-not (Test-AbsPath $p)) { Write-Warning "An absolute path is required, e.g. D:\AI\llama"; continue }
    $problem = & $check $p
    if (-not $problem) { return $p }
    Write-Warning $problem
    if ((Read-Host '  Keep this path anyway? (y = yes, Enter = type again)') -match '^[yY]') { return $p }
  }
}

# Путь к llama-server.exe по ответу: папка → папка\llama-server.exe; путь к .exe — как есть.
function Resolve-LlamaExe([string]$p) { if ($p -match '\.exe$') { $p } else { Join-Path $p 'llama-server.exe' } }

# 0. Пути к llama.cpp и моделям — нужны только для нового конфига. Спрашиваем до сборки, чтобы не ждать.
$newCfg = -not (Test-Path $cfg)
$ask = $newCfg -and [Environment]::UserInteractive -and -not [Console]::IsInputRedirected
if ($LlamaDir) { $LlamaExe = Resolve-LlamaExe (Clean-Path $LlamaDir) }
if ($newCfg -and -not $LlamaExe) {
  if ($ask) {
    $dir = Ask-Path 'Absolute path to the llama.cpp folder (the one with llama-server.exe):' 'C:\llama' {
      param($p) $e = Resolve-LlamaExe $p; if (-not (Test-Path -LiteralPath $e -PathType Leaf)) { "Not found: $e" } }
    $LlamaExe = Resolve-LlamaExe $dir
  } else { $LlamaExe = 'C:\llama\llama-server.exe' }
}
if ($newCfg -and -not $ModelsDir) {
  if ($ask) {
    $ModelsDir = Ask-Path 'Absolute path to the models folder (.gguf files):' 'C:\models' {
      param($p) if (-not (Test-Path -LiteralPath $p -PathType Container)) { "Folder not found: $p" } }
  } else { $ModelsDir = 'C:\models' }
}
if ($LlamaExe) { $LlamaExe = Clean-Path $LlamaExe }
if ($ModelsDir) { $ModelsDir = Clean-Path $ModelsDir }
if ($newCfg) {
  if (-not (Test-AbsPath $LlamaExe)) { throw "The path to llama-server.exe must be absolute: $LlamaExe" }
  if (-not (Test-AbsPath $ModelsDir)) { throw "The models folder path must be absolute: $ModelsDir" }
} elseif ($LlamaDir -or $LlamaExe -or $ModelsDir) {
  Write-Warning "Config already exists, path parameters are ignored. Edit server_exe and models_dirs in $cfg"
}

# 1. Сборка. Ошибка — до того, как что-то изменилось на ПК.
& (Join-Path $PSScriptRoot 'build.ps1') | Out-Null
$built = Join-Path $root 'bin\locallm-host.exe'

# 2. Остановить работающий хост: exe занят, пока он запущен. llama-server завершит Job Object.
$running = Get-Process locallm-host -ErrorAction SilentlyContinue
if ($running) {
  'Stopping the running host…'
  $running | Stop-Process -Force
  $running | Wait-Process -Timeout 15 -ErrorAction SilentlyContinue
}

# 3. Программа.
New-Item -ItemType Directory -Force $appDir | Out-Null
Copy-Item $built $exe -Force
"Program: $exe"

# 4. Конфиг — только если его ещё нет.
New-Item -ItemType Directory -Force $cfgDir | Out-Null
if (Test-Path $cfg) {
  "Config already exists, leaving it as is: $cfg"
} else {
  if (-not (Test-Path -LiteralPath $LlamaExe)) { Write-Warning "$LlamaExe not found — fix server_exe in $cfg" }
  if (-not (Test-Path -LiteralPath $ModelsDir)) { Write-Warning "Folder $ModelsDir not found — fix models_dirs in $cfg" }
  $text = Get-Content -Raw (Join-Path $root 'locallm.example.yaml')
  $text = $text -replace "(?m)^(  server_exe: )'[^']*'", ('$1' + (Quote $LlamaExe).Replace('$', '$$'))
  $text = $text -replace "(?m)^(  default_model: )qwen-coder", "`$1''"
  $text = $text -replace "(?m)^models_dirs: \[[^\]]*\]", ('models_dirs: [' + (Quote $ModelsDir).Replace('$', '$$') + ']')
  $text = $text -replace "(?m)^# locallm\.example\.yaml.*\r?\n# Скопируйте.*\r?\n", "# locallm.yaml — общие настройки хоста; создан scripts\install.ps1.`n"
  [IO.File]::WriteAllText($cfg, $text, [Text.UTF8Encoding]::new($false))
  "Config: $cfg"
}
if (-not (Test-Path $models)) {
  [IO.File]::WriteAllText($models, "# models.yaml — модели locallm-host. Добавляйте их в панели: «Найти .gguf и добавить модель».`nversion: 1`nmodels: {}`n", [Text.UTF8Encoding]::new($false))
  "Model list (empty for now): $models"
}

# 5. Ярлык в «Пуске».
$sh = New-Object -ComObject WScript.Shell
$l = $sh.CreateShortcut($lnk)
$l.TargetPath = $exe
$l.WorkingDirectory = $appDir
$l.Description = 'locallm-host: local llama.cpp models'
$l.Save()
"Shortcut: Start → locallm"

# 6. Запуск. Второй запуск того же хоста открывает панель (так устроен exe).
if (-not $NoStart) {
  Start-Process $exe
  $up = $false
  for ($i = 0; $i -lt 30 -and -not $up; $i++) {
    Start-Sleep -Milliseconds 500
    try { $up = (Invoke-RestMethod http://127.0.0.1:8080/health -TimeoutSec 2).status -eq 'ok' } catch { }
  }
  if ($up) {
    Start-Process $exe
    'Host is running: icon next to the clock, the panel opens in the browser.'
  } else {
    Write-Warning "Host did not respond within 15 s. Log: $cfgDir\logs\host.log"
  }
}
