@echo off
rem BlockMemoryAgent launcher (visible console, for debugging: BMA.cmd -NoBrowser)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0app\launch.ps1" %*
exit /b %ERRORLEVEL%
