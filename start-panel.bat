@echo off
chcp 65001 >nul
title WorkBuddy2API Panel

cd /d "%~dp0"

if not exist "config.json" (
    echo [ERROR] config.json not found. Copy config.example.json to config.json first.
    pause
    exit /b 1
)

if not exist "wb2api.exe" (
    echo [ERROR] wb2api.exe not found in this folder.
    pause
    exit /b 1
)

echo ============================================
echo   WorkBuddy2API Gateway
echo   Panel: http://127.0.0.1:7863/panel/
echo   Press Ctrl+C to stop
echo ============================================
echo.

wb2api.exe -config config.json

echo.
echo [EXIT] service stopped
pause
