@echo off
chcp 65001 >nul
cd /d "%~dp0"
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "Get-ChildItem -LiteralPath '%~dp0' -Filter '*.exe' | Unblock-File -ErrorAction SilentlyContinue"
if /I "%PROCESSOR_ARCHITECTURE%"=="x86" (
  powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~dp0HdRecover_32bits.exe' -Verb RunAs"
) else (
  powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~dp0HdRecover.exe' -Verb RunAs"
)
