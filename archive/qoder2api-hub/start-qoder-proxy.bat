@echo off
setlocal EnableExtensions
rem ===========================================================
rem  Qoder proxy - launcher (CN + Intl)  ** detached mode **
rem  Usage: double-click, or  start-qoder-proxy.bat [port]
rem
rem  issue #50: the server now runs DETACHED. Closing this
rem  window no longer stops it; use stop-qoder-proxy.bat.
rem
rem  ASCII-only on purpose: .bat files are read using the
rem  console code page, so non-ASCII text breaks the parser.
rem ===========================================================

set "PORT=%~1"
if "%PORT%"=="" set "PORT=8790"
set "HERE=%~dp0"
set "SCRIPT=%HERE%qoder_proxy.py"
set "PIDFILE=%HERE%qoder-proxy.pid"
set "LOGFILE=%HERE%qoder-proxy.log"
set "ERRFILE=%HERE%qoder-proxy.err.log"

if not exist "%SCRIPT%" goto no_script

set "PYEXE="

if not exist "%HERE%python\python.exe" goto try_path
"%HERE%python\python.exe" --version >nul 2>nul
if errorlevel 1 goto try_path
set "PYEXE=%HERE%python\python.exe"
goto check_running

:try_path
python --version >nul 2>nul
if errorlevel 1 goto try_py
set "PYEXE=python"
goto check_running

:try_py
py --version >nul 2>nul
if errorlevel 1 goto no_python
set "PYEXE=py"
goto check_running

:no_python
echo [ERROR] No usable Python found.
echo   Use the packaged zip (contains python\), or install Python 3.9+.
if not "%QD_NO_PAUSE%"=="1" pause
exit /b 1

:no_script
echo [ERROR] qoder_proxy.py not found next to this script.
if not "%QD_NO_PAUSE%"=="1" pause
exit /b 1

:check_running
rem Windows reuses PIDs, so "the PID exists" proves nothing: it may now
rem belong to some unrelated process (an Edge WebView child, for example).
rem That made this script print "[INFO] already running" and skip the
rem launch while nothing was listening on the port. Verify the command
rem line really is qoder_proxy.py, the same way stop-qoder-proxy.bat does.
rem
rem Exit codes: 0 = the same instance is already up on this port,
rem             1 = not running (or stale pid) -> go launch,
rem             2 = running on a DIFFERENT port.
rem
rem Case 2 used to be silently mislabelled as "already running" and the
rem user was handed a Dashboard URL for a port nothing was serving. It
rem must NOT fall through to launch either: qoder-proxy.pid is a single
rem slot, so a second instance would overwrite it and stop-qoder-proxy.bat
rem would lose track of the first one. Refuse and say so instead.
if not exist "%PIDFILE%" goto launch
set /p OLDPID=<"%PIDFILE%"
if "%OLDPID%"=="" goto launch
rem A hand-edited or half-written pid file can hold anything, so parse it
rem instead of casting: [int]'garbage' throws a red PowerShell error at
rem the user even though the outcome is the right one (go launch).
powershell -NoProfile -ExecutionPolicy Bypass -Command "$id = 0; if (-not [int]::TryParse(('%OLDPID%').Trim(), [ref]$id)) { exit 1 }; $want = '%PORT%'; $p = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $id) -ErrorAction SilentlyContinue; if (-not $p) { exit 1 }; if ($p.CommandLine -notmatch 'qoder_proxy\.py') { exit 1 }; if ($p.CommandLine -notmatch ('--port\s+' + [regex]::Escape($want) + '(\s|$)')) { exit 2 }; exit 0"
if errorlevel 2 goto running_other_port
if errorlevel 1 goto launch
echo [INFO] qoder-proxy already running (PID %OLDPID%) on port %PORT%.
echo        Dashboard: http://127.0.0.1:%PORT%/
echo        Stop it with: stop-qoder-proxy.bat
goto end

:running_other_port
echo [WARN] qoder-proxy is already running as PID %OLDPID%, but on a
echo        different port than %PORT% - so nothing is serving
echo        http://127.0.0.1:%PORT%/ right now.
echo.
echo        Only one instance is tracked at a time (single pid file),
echo        so starting a second one would make stop-qoder-proxy.bat
echo        lose track of the first. Stop it first, then start again:
echo           stop-qoder-proxy.bat
echo           start-qoder-proxy.bat %PORT%
echo.
if not "%QD_NO_PAUSE%"=="1" pause
exit /b 1

:launch
echo Starting qoder-proxy on port %PORT% (detached) ...
rem issue #85: -ArgumentList elements are NOT auto-quoted, so a path with
rem spaces gets split into several argv entries. Build the quotes with
rem [char]34 (avoids bat/powershell double-escaping) and pass paths via
rem environment variables (avoids cmd metacharacters like & and ^).
set "QD_EXE=%PYEXE%"
set "QD_SCRIPT=%SCRIPT%"
set "QD_PIDFILE=%PIDFILE%"
set "QD_LOGFILE=%LOGFILE%"
set "QD_ERRFILE=%ERRFILE%"
set "QD_PORT=%PORT%"
rem Two Windows/PowerShell 5.1 traps are handled below.
rem
rem (1) Duplicate proxy env vars. Tools like Clash/Node often export BOTH
rem     "HTTP_PROXY" and "http_proxy". PowerShell 5.1's Start-Process
rem     copies the environment into a case-SENSITIVE dictionary, so the
rem     second spelling throws
rem       "Item has already been added. Key in dictionary: 'HTTP_PROXY'"
rem     and NOTHING gets started. Dropping the lowercase copies is safe:
rem     Python's os.environ is case-insensitive on Windows and folds
rem     everything to uppercase, so the proxy settings survive.
rem
rem (2) Start-Process failures used to be swallowed: a $null $p made the
rem     old one-liner print "[OK] started, PID " with an empty PID, and
rem     the script exited 0 while no port was listening. Now any failure
rem     exits non-zero, and success requires the port to actually accept
rem     a TCP connection.
rem
rem     Note: do NOT verify with Get-NetTCPConnection -State Listen here.
rem     On this machine that filter silently misses real listeners (it
rem     reported 0 rows for a port that was serving HTTP 200), so it would
rem     reproduce the very false-negative this check exists to prevent.
rem     A real connect() tests exactly what the user cares about.
powershell -NoProfile -ExecutionPolicy Bypass -Command "$q = [char]34; foreach ($n in @('http_proxy','https_proxy','no_proxy','all_proxy')) { if ([Environment]::GetEnvironmentVariable($n, 'Process')) { [Environment]::SetEnvironmentVariable($n, $null, 'Process') } }; $argList = @(($q + $env:QD_SCRIPT + $q), '--port', $env:QD_PORT, '--pid-file', ($q + $env:QD_PIDFILE + $q)); $p = $null; try { $p = Start-Process -FilePath $env:QD_EXE -ArgumentList $argList -WindowStyle Hidden -RedirectStandardOutput $env:QD_LOGFILE -RedirectStandardError $env:QD_ERRFILE -PassThru } catch { Write-Host ('[ERROR] could not start: ' + $_.Exception.Message); exit 1 }; Start-Sleep -Milliseconds 1200; if (-not $p) { Write-Host '[ERROR] Start-Process returned no process object'; exit 1 }; if ($p.HasExited) { Write-Host '[ERROR] exited immediately after spawn - see qoder-proxy.err.log'; exit 1 }; $portNum = [int]$env:QD_PORT; $listening = $false; for ($i = 0; $i -lt 30; $i++) { $tc = New-Object System.Net.Sockets.TcpClient; try { $tc.Connect('127.0.0.1', $portNum); $listening = $true } catch { } finally { $tc.Close() }; if ($listening) { break }; if ($p.HasExited) { Write-Host '[ERROR] process died while waiting for the port - see qoder-proxy.err.log'; exit 1 }; Start-Sleep -Milliseconds 500 }; if (-not $listening) { Write-Host ('[ERROR] PID ' + $p.Id + ' is alive but port ' + $env:QD_PORT + ' never accepted a connection - see qoder-proxy.err.log'); exit 2 }; Write-Host ('[OK] started, PID ' + $p.Id)"
if errorlevel 2 goto launch_no_port
if errorlevel 1 goto launch_failed
echo.
echo   API      : http://127.0.0.1:%PORT%/v1
echo   Dashboard: http://127.0.0.1:%PORT%/
echo   Log      : %LOGFILE%
echo.
echo   This window can be closed now - the server keeps running.
echo   Stop it with: stop-qoder-proxy.bat
goto :end

:launch_failed
echo.
echo [ERROR] qoder-proxy did NOT start - see qoder-proxy.err.log above.
echo.
if not "%QD_NO_PAUSE%"=="1" pause
exit /b 1

:launch_no_port
echo.
echo [ERROR] qoder-proxy process is alive but the port never opened.
echo         Check qoder-proxy.err.log - the port may already be taken
echo         by another program, or startup crashed after spawning.
echo.
if not "%QD_NO_PAUSE%"=="1" pause
exit /b 1

:end
echo.
if not "%QD_NO_PAUSE%"=="1" pause
