@echo off
title BMA media apply
chcp 65001 >nul
cd /d "%~dp0"

rem Apply repo-root .env media section to the open_design daemon.
rem Equivalent to:  bash docker/bma-plugin.sh media
set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=%ProgramFiles(x86)%\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=%LocalAppData%\Programs\Git\bin\bash.exe"
if not exist "%BASH%" (
  echo [ERROR] Git Bash not found. Install Git for Windows first,
  echo         or open Git Bash manually and run: bash docker/bma-plugin.sh media
  pause
  exit /b 1
)

echo Applying media config from repo .env to open_design ...
echo.
"%BASH%" docker/bma-plugin.sh media

echo.
echo All done. Press any key to close.
pause >nul
