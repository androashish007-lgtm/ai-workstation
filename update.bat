@echo off
setlocal enabledelayedexpansion
rem Fetches the latest published VERSION and this platform's binary from the
rem repo's default branch and replaces the local copies. There's no CI/release
rem pipeline for this project yet -- binaries live directly in bin/ on the
rem default branch rather than as GitHub Release assets, so this pulls the
rem raw files directly instead of using the GitHub Releases API.

set "DIR=%~dp0"
if "%DIR:~-1%"=="\" set "DIR=%DIR:~0,-1%"
cd /d "%DIR%"

set "REPO_RAW=https://raw.githubusercontent.com/androashish007-lgtm/ai-workstation/master"

set "ARCH=amd64"
if /I "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "ARCH=arm64"
if defined PROCESSOR_ARCHITEW6432 (
  if /I "%PROCESSOR_ARCHITEW6432%"=="ARM64" set "ARCH=arm64"
)
set "BIN_NAME=aistation-windows-%ARCH%.exe"

where curl >nul 2>nul
if errorlevel 1 (
  echo Need curl.exe to check for updates ^(ships with Windows 10 1803+ and Windows 11^).
  pause
  exit /b 1
)

set "CURRENT_VERSION=unknown"
if exist VERSION set /p CURRENT_VERSION=<VERSION

curl -fsSL "%REPO_RAW%/VERSION" -o "%TEMP%\aistation_latest_version.txt"
if errorlevel 1 (
  echo Could not reach GitHub to check for updates.
  pause
  exit /b 1
)
set /p LATEST_VERSION=<"%TEMP%\aistation_latest_version.txt"
del "%TEMP%\aistation_latest_version.txt" >nul 2>nul

if "%LATEST_VERSION%"=="%CURRENT_VERSION%" (
  echo Already up to date ^(v%CURRENT_VERSION%^).
  pause
  exit /b 0
)

echo Updating v%CURRENT_VERSION% -^> v%LATEST_VERSION%...
curl -fsSL "%REPO_RAW%/bin/%BIN_NAME%" -o "bin\%BIN_NAME%.new"
if errorlevel 1 (
  echo Download failed.
  pause
  exit /b 1
)
rem Renaming over the old exe works even while it's currently running --
rem Windows keeps the running process's own handle to the old file data.
move /Y "bin\%BIN_NAME%.new" "bin\%BIN_NAME%" >nul
echo %LATEST_VERSION%> VERSION
echo Updated to v%LATEST_VERSION%. Run start.bat to launch it.
pause
