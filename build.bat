@echo off
rem One-shot build. Outputs:
rem   ..\wb2api.exe              server (scheduled task)
rem   ..\dist\WorkBuddy2API.exe  launcher (open panel, --server/--stop)
rem Keep flags identical for both outputs: -trimpath -s -w -H=windowsgui
setlocal
cd /d "%~dp0"
set GOROOT=D:\gotool\go
set CGO_ENABLED=0
set GOPROXY=https://goproxy.cn,direct
set PATH=%GOROOT%\bin;%PATH%

echo [1/3] run tests...
go test ./internal/... -count=1
if errorlevel 1 goto err

echo [2/3] build server ..\wb2api.exe (cmd/server)
go build -trimpath -ldflags "-s -w -H=windowsgui" -o ..\wb2api.exe .\cmd\server
if errorlevel 1 goto err

echo [3/3] build launcher ..\dist\WorkBuddy2API.exe (cmd/app)
go build -trimpath -ldflags "-s -w -H=windowsgui" -o ..\dist\WorkBuddy2API.exe .\cmd\app
if errorlevel 1 goto err

echo.
echo OK. restart service: taskkill /IM wb2api.exe /F then schtasks /run /TN WorkBuddy2API
exit /b 0

:err
echo.
echo BUILD FAILED (no artifact overwritten)
exit /b 1