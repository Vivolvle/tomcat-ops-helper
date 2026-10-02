@echo off
setlocal

if not defined GOROOT (
  where go >nul 2>nul
  if errorlevel 1 (
    echo Go is not in PATH. Install Go 1.21+ first.
    exit /b 1
  )
)

if not exist bin mkdir bin

echo === building helper ===
go build -trimpath -ldflags="-s -w" -o bin\helper.exe .\cmd\helper
if errorlevel 1 exit /b 1

echo === building server ===
go build -trimpath -ldflags="-s -w" -o bin\server.exe .\cmd\server
if errorlevel 1 exit /b 1

echo === building helper-gui ===
go build -trimpath -ldflags="-s -w" -o bin\helper-gui.exe .\cmd\helper-gui
if errorlevel 1 exit /b 1

echo === running tests ===
go test ./...
if errorlevel 1 exit /b 1

echo === done ===
echo Binaries:
dir /b bin
endlocal
