@echo off
rem 一键构建（唯一正确姿势，别再手敲 go build）
rem   ..\wb2api.exe              服务端（计划任务用）
rem   ..\dist\WorkBuddy2API.exe  分发包启动器（双击开面板，带 --server/--stop）
rem 两个产物参数必须一致：-trimpath -s -w -H=windowsgui（否则会弹黑窗、体积虚高）。
setlocal
cd /d "%~dp0"
set GOROOT=D:\gotool\go
set CGO_ENABLED=0
set GOPROXY=https://goproxy.cn,direct
set PATH=%GOROOT%\bin;%PATH%

echo [1/3] 全量测试...
go test ./internal/... -count=1
if errorlevel 1 goto err

echo [2/3] 构建服务端 ..\wb2api.exe (cmd/server)
go build -trimpath -ldflags "-s -w -H=windowsgui" -o ..\wb2api.exe .\cmd\server
if errorlevel 1 goto err

echo [3/3] 构建分发包启动器 ..\dist\WorkBuddy2API.exe (cmd/app)
go build -trimpath -ldflags "-s -w -H=windowsgui" -o ..\dist\WorkBuddy2API.exe .\cmd\app
if errorlevel 1 goto err

echo.
echo OK. 记得重启服务：taskkill /IM wb2api.exe /F 然后 schtasks /run /TN WorkBuddy2API
exit /b 0

:err
echo.
echo BUILD FAILED（未覆盖任何产物）
exit /b 1
