@echo off
echo Building Windows Parental Control Bot...

set GOOS=windows
set GOARCH=amd64

go build -o parental-control-bot.exe

if %errorlevel% equ 0 (
    echo ✅ Build successful! Created parental-control-bot.exe
    echo.
    echo Next steps:
    echo 1. Copy config.json.example to config.json and configure it
    echo 2. Run as Administrator: parental-control-bot.exe -install
    echo 3. The service will start automatically
) else (
    echo ❌ Build failed!
    exit /b 1
)
