@echo off
setlocal
rem Single-step install+configure+launch for Windows.
rem Everything lives inside this folder -- nothing is installed system-wide.

set "DIR=%~dp0"
if "%DIR:~-1%"=="\" set "DIR=%DIR:~0,-1%"

set "ARCH=amd64"
if /I "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "ARCH=arm64"
if defined PROCESSOR_ARCHITEW6432 (
  if /I "%PROCESSOR_ARCHITEW6432%"=="ARM64" set "ARCH=arm64"
)

set "BIN=%DIR%\bin\aistation-windows-%ARCH%.exe"

if not exist "%BIN%" (
  echo No prebuilt binary found at %BIN%
  echo See TROUBLESHOOTING.md for how to build one from src\ with a portable Go toolchain.
  pause
  exit /b 1
)

"%BIN%" --dir "%DIR%" %*
