# Generates a self-signed cert for local testing under .\certs\.
# Run once before starting server.exe.
# Outputs PEM-formatted server.pem and server.key.

$ErrorActionPreference = "Stop"
$certDir = Join-Path $PSScriptRoot "certs"
New-Item -ItemType Directory -Force -Path $certDir | Out-Null

$certPem = Join-Path $certDir "server.pem"
$keyPem  = Join-Path $certDir "server.key"

if ((Test-Path $certPem) -and (Test-Path $keyPem)) {
    Write-Host "Cert already exists at $certDir"
    exit 0
}

# Use openssl if available; otherwise fall back to PowerShell's
# New-SelfSignedCertificate + manual PEM export.
$openssl = (Get-Command openssl -ErrorAction SilentlyContinue).Source
if ($openssl) {
    & openssl req -x509 -newkey rsa:2048 -nodes -days 365 `
        -keyout $keyPem -out $certPem `
        -subj "/CN=localhost" `
        -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
    Write-Host "Generated via openssl:"
    Write-Host "  $certPem"
    Write-Host "  $keyPem"
    exit 0
}

Write-Host "openssl not found; using New-SelfSignedCertificate..."

$pfx = Join-Path $certDir "server.pfx"
$cert = New-SelfSignedCertificate `
    -Subject "CN=localhost" `
    -DnsName "localhost" `
    -CertStoreLocation "Cert:\CurrentUser\My" `
    -KeyAlgorithm RSA -KeyLength 2048 `
    -NotAfter (Get-Date).AddDays(365) `
    -KeyUsage DigitalSignature,KeyEncipherment `
    -KeyExportPolicy Exportable `
    -KeySpec Signature `
    -FriendlyName "tomcat-ops-helper local"

$thumb = $cert.Thumbprint
$store = Get-ChildItem "Cert:\CurrentUser\My\$thumb"
$password = ConvertTo-SecureString -String "tomcat-ops-helper" -Force -AsPlainText
Export-PfxCertificate -Cert $store -FilePath $pfx -Password $password | Out-Null

# Export PEM (cert) and KEY. We use .NET directly because PowerShell does not
# expose PEM export out of the box.
$loaded = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2(
    $pfx, "tomcat-ops-helper",
    [System.Security.Cryptography.X509Certificates.X509KeyStorageFlags]::Exportable)
[System.IO.File]::WriteAllText($certPem, (
    "-----BEGIN CERTIFICATE-----`n" +
    [Convert]::ToBase64String($loaded.Export([System.Security.Cryptography.X509Certificates.X509ContentType]::Cert), "InsertLineBreaks") +
    "`n-----END CERTIFICATE-----`n"
))

$rsa = $loaded.PrivateKey
if ($null -eq $rsa) {
    throw "No private key on cert"
}
$keyBytes = $rsa.ExportRSAPrivateKey()
[System.IO.File]::WriteAllText($keyPem, (
    "-----BEGIN RSA PRIVATE KEY-----`n" +
    [Convert]::ToBase64String($keyBytes, "InsertLineBreaks") +
    "`n-----END RSA PRIVATE KEY-----`n"
))

Write-Host "Generated via PowerShell:"
Write-Host "  $certPem"
Write-Host "  $keyPem"
Write-Host "Cert is also installed in Cert:\CurrentUser\My ($thumb). To remove: Remove-Item Cert:\CurrentUser\My\$thumb"
