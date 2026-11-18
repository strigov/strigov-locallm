# scripts/build.ps1 — сборки §4.8: release-хост без консоли и debug-хост с консолью (для автоматических прогонов), fakeupstream и ggufinfo (QV4).
# Хост собирается первым: ошибка во вспомогательной программе не оставляет bin\ без нового хоста (F1-D2).
# Хосту нужен fyne.io/systray (решение S8): первая сборка на чистом ПК скачивает модуль, дальше он берётся из кэша Go.
$ErrorActionPreference = 'Stop'
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { $env:Path = "$env:LOCALAPPDATA\Programs\go\bin;C:\Program Files\Go\bin;$env:Path" }
$env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
$root = Split-Path -Parent $PSScriptRoot
function Invoke-Go { & go @args; if ($LASTEXITCODE -ne 0) { throw "go $($args -join ' ') failed with $LASTEXITCODE" } }
Push-Location $root
try {
  New-Item -ItemType Directory -Force bin | Out-Null
  if (Test-Path .\cmd\locallm-host) {
    Invoke-Go build -trimpath -ldflags "-H=windowsgui -s -w -X main.version=0.2.4" -o bin\locallm-host.exe .\cmd\locallm-host
    Invoke-Go build -trimpath -ldflags "-X main.version=0.2.4-debug" -o bin\locallm-host-debug.exe .\cmd\locallm-host
  }
  Invoke-Go build -trimpath -o bin\fakeupstream.exe .\cmd\fakeupstream
  Invoke-Go build -trimpath -o bin\ggufinfo.exe .\cmd\ggufinfo
  Get-ChildItem bin\*.exe | Select-Object Name, Length
} finally { Pop-Location }
