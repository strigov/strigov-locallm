# scripts/uninstall.ps1 — удаление locallm-host, установленного scripts\install.ps1.
# Останавливает хост, убирает exe, ярлык, автозапуск и выдачу в tailnet (tailscale serve на порту 8080).
# Конфиг, ключи и журналы (%LOCALAPPDATA%\locallm) остаются; -Purge удаляет и их.
# Код (папка репозитория), llama.cpp и модели не трогаются.
param([switch]$Purge)
$ErrorActionPreference = 'Stop'
$appDir = Join-Path $env:LOCALAPPDATA 'Programs\locallm'
$cfgDir = Join-Path $env:LOCALAPPDATA 'locallm'
$lnk = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\locallm.lnk'

$running = Get-Process locallm-host -ErrorAction SilentlyContinue
if ($running) {
  $running | Stop-Process -Force
  $running | Wait-Process -Timeout 15 -ErrorAction SilentlyContinue
  'Host stopped.'
}

foreach ($key in 'Run', 'Explorer\StartupApproved\Run') {
  Remove-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\$key" -Name locallm-host -ErrorAction SilentlyContinue
}
'Autostart disabled.'

if (Get-Command tailscale -ErrorAction SilentlyContinue) {
  tailscale serve --http=8080 off 2>$null | Out-Null # нет выдачи — код 1, это не ошибка
  if ($LASTEXITCODE -eq 0) { 'Tailnet serving on port 8080 removed.' }
}

Remove-Item $lnk -Force -ErrorAction SilentlyContinue
for ($i = 0; (Test-Path $appDir) -and $i -lt 10; $i++) { # exe отпускается не сразу после остановки
  try { Remove-Item $appDir -Recurse -Force } catch { Start-Sleep -Milliseconds 500 }
}
if (Test-Path $appDir) { Write-Warning "Could not remove $appDir — delete it manually" } else { 'Program removed.' }

if ($Purge) {
  Remove-Item $cfgDir -Recurse -Force -ErrorAction SilentlyContinue
  "Settings, keys and logs removed: $cfgDir"
} elseif (Test-Path $cfgDir) {
  "Settings, keys and logs kept: $cfgDir (to remove: uninstall.ps1 -Purge)"
}
