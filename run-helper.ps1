# Boots helper.exe with config.json from this directory.
# helper.exe is built with a console so logs stream here. If you want a
# no-console variant for stealth, rebuild with:
#   go build -ldflags="-H windowsgui -s -w" -o bin\helper.exe .\cmd\helper

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
& ".\bin\helper.exe" -config ".\config.json"
