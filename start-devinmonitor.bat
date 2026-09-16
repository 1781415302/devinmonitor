@echo off
setlocal
title DevinMonitor

set "APP_DIR=%~dp0"
set "EXE=%APP_DIR%devinmonitor.exe"
set "PORT=19191"
set "URL=http://127.0.0.1:%PORT%/"

cd /d "%APP_DIR%"

if exist "%EXE%" goto :check_ready

echo Building devinmonitor.exe ...
set "GOPROXY=https://goproxy.cn,direct"
set "GOSUMDB=sum.golang.google.cn"
set "PATH=C:\Program Files\Go\bin;%PATH%"
go build -o "%EXE%" .
if errorlevel 1 (
  echo Build failed. Install Go or place devinmonitor.exe in this folder.
  pause
  exit /b 1
)

:check_ready
powershell -NoProfile -ExecutionPolicy Bypass -Command "try { Invoke-WebRequest -Uri '%URL%' -UseBasicParsing -TimeoutSec 2 | Out-Null; exit 0 } catch { exit 1 }"
if not errorlevel 1 (
  start "" "%URL%"
  exit /b 0
)

powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-Process devinmonitor -ErrorAction SilentlyContinue | Stop-Process -Force"

echo Starting DevinMonitor web dashboard on %URL% ...
start "" /b "%EXE%" web --port %PORT%

set /a WAIT_N=0
:wait_ready
timeout /t 1 /nobreak >nul
powershell -NoProfile -ExecutionPolicy Bypass -Command "try { Invoke-WebRequest -Uri '%URL%' -UseBasicParsing -TimeoutSec 2 | Out-Null; exit 0 } catch { exit 1 }"
if not errorlevel 1 (
  start "" "%URL%"
  echo Started: %URL%
  timeout /t 2 >nul
  exit /b 0
)
set /a WAIT_N+=1
if %WAIT_N% lss 20 goto :wait_ready

echo Start timeout. Check %EXE%
pause
exit /b 1
