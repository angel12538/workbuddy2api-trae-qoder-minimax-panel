@echo off
setlocal
title Build Trae Work Assistant (Release)

cd /d "%~dp0"

echo ============================================
echo   Trae Work Assistant - Release Build
echo ============================================
echo.

echo [1/3] Loading MSVC x64 environment...
call "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars64.bat"
if errorlevel 1 (
    echo [ERROR] vcvars64.bat failed. Is VS Build Tools (C++ workload) installed?
    pause
    exit /b 1
)

echo.
echo [2/3] Checking frontend dependencies...
if not exist "node_modules" (
    echo node_modules missing, running npm install...
    call npm install
    if errorlevel 1 ( echo [ERROR] npm install failed & pause & exit /b 1 )
)

echo.
echo [3/3] Building Tauri release (first run takes 15-30 min)...
call npm run tauri build
if errorlevel 1 (
    echo.
    echo [ERROR] Build failed. Scroll up for details.
    pause
    exit /b 1
)

echo.
echo ============================================
echo   BUILD OK
echo.
echo   Standalone exe:
echo     src-tauri\target\release\*.exe
echo   Installers:
echo     src-tauri\target\release\bundle\msi\*.msi
echo     src-tauri\target\release\bundle\nsis\*-setup.exe
echo ============================================
pause
