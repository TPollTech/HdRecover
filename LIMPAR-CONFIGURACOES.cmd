@echo off
chcp 65001 >nul
del /q "%LOCALAPPDATA%\HdRecover\settings.json" 2>nul
echo Configurações locais removidas.
pause
