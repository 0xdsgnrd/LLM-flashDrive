@echo off
REM Portable LLM launcher - Windows. Delegates to PowerShell for RAM detection.
REM -ExecutionPolicy Bypass avoids the .ps1 script block WITHOUT needing admin.
powershell -ExecutionPolicy Bypass -NoProfile -File "%~dp0run-windows.ps1"
if errorlevel 1 pause
