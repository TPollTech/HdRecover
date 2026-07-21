@echo off
chcp 65001 >nul
set "LOG=%LOCALAPPDATA%\HdRecover\HdRecover_startup.log"
if exist "%LOG%" (start "" notepad.exe "%LOG%") else (echo O log ainda não existe: %LOG% & pause)
