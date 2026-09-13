# Boots server.exe with server.json from this directory. Open in a separate
# terminal; Ctrl+C stops it.

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
& ".\bin\server.exe" -config ".\server.json"
