#!/bin/bash
# Build script for Windows Parental Control Bot

echo "Building Windows Parental Control Bot..."

# Set environment for Windows build
export GOOS=windows
export GOARCH=amd64

# Build the executable
go build -o parental-control-bot.exe

if [ $? -eq 0 ]; then
    echo "✅ Build successful! Created parental-control-bot.exe"
    echo ""
    echo "Next steps:"
    echo "1. Copy parental-control-bot.exe to your Windows machine"
    echo "2. Copy config.json.example to config.json and configure it"
    echo "3. Run as Administrator: parental-control-bot.exe -install"
    echo "4. The service will start automatically"
else
    echo "❌ Build failed!"
    exit 1
fi
