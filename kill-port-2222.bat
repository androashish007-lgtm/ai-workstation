@echo off
setlocal EnableDelayedExpansion

:: ---------------------------------------------------------
:: kill-port-2222.bat
:: Lists every process holding port 2222, then kills them.
:: Run as Administrator if the owning process is a service.
:: Change PORT below to reuse this for any other port.
:: ---------------------------------------------------------

set "PORT=2222"
set "PIDLIST="

echo.
echo Scanning for processes on port %PORT% ...
echo.

for /f "tokens=1-6" %%a in ('netstat -ano ^| findstr /r /c:":%PORT%[^0-9]" /c:":%PORT%$"') do (
    set "PROTO=%%a"
    set "PID="
    if /i "!PROTO!"=="TCP" (
        set "PID=%%e"
        set "STATE=%%d"
    ) else (
        set "PID=%%d"
        set "STATE=UDP"
    )
    if defined PID (
        echo !PIDLIST! | findstr /c:"[!PID!]" >nul
        if errorlevel 1 (
            set "PIDLIST=!PIDLIST![!PID!]"
            for /f "tokens=1 delims=," %%n in ('tasklist /fi "PID eq !PID!" /fo csv /nh 2^>nul') do set "PNAME=%%~n"
            if not defined PNAME set "PNAME=unknown"
            echo   PID !PID!  ^|  !PNAME!  ^|  !PROTO! !STATE!  ^|  %%b
            set "PNAME="
        )
    )
)

if not defined PIDLIST (
    echo   Nothing is listening on port %PORT%.
    echo.
    goto :end
)

echo.
set /p CONFIRM=Kill the process(es) listed above? [Y/N] 
if /i not "%CONFIRM%"=="Y" (
    echo Aborted. Nothing was killed.
    goto :end
)

echo.
set "PIDLIST=!PIDLIST:][= !"
set "PIDLIST=!PIDLIST:[=!"
set "PIDLIST=!PIDLIST:]=!"

for %%p in (!PIDLIST!) do (
    taskkill /PID %%p /F /T >nul 2>&1
    if errorlevel 1 (
        echo   Failed to kill PID %%p. Try running this script as Administrator.
    ) else (
        echo   Killed PID %%p.
    )
)

echo.
echo Re-checking port %PORT% ...
netstat -ano | findstr /r /c:":%PORT%[^0-9]" /c:":%PORT%$" >nul
if errorlevel 1 (
    echo   Port %PORT% is now free.
) else (
    echo   Port %PORT% is still in use. Some sockets may be in TIME_WAIT.
)

:end
echo.
pause
endlocal
