# Start trace-portal for local development.
#
#   .\scripts\dev.ps1            # local files only, no Docker needed
#   .\scripts\dev.ps1 -Stack     # against the Postgres + MinIO in docker compose
#   .\scripts\dev.ps1 -Stop      # stop a running instance
#
# Leaves the server running in the background and prints the URL. Nothing here
# is needed to use the tool -- `go run ./cmd/trace-portal` is the whole product.
# This exists so the Postgres/MinIO configuration is one flag instead of six
# environment variables remembered correctly.

[CmdletBinding()]
param(
    # Use the docker compose Postgres and MinIO instead of local files.
    [switch]$Stack,

    # Stop a running instance and exit.
    [switch]$Stop,

    # Data directory. The default is the real archive; -Stack uses a separate
    # one so experimenting against a database never touches it.
    [string]$Data,

    [int]$Port = 8317
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Stop-TracePortal {
    $running = Get-Process trace-portal -ErrorAction SilentlyContinue
    if ($running) { $running | Stop-Process -Force; Write-Host "Stopped trace-portal." }
    else { Write-Host "trace-portal was not running." }
}

if ($Stop) { Stop-TracePortal; return }

if (-not $Data) {
    $Data = if ($Stack) { Join-Path $env:TEMP 'tp-stack-data' } else { Join-Path $env:USERPROFILE '.trace-portal' }
}

Write-Host "Building..."
& go build -o trace-portal.exe ./cmd/trace-portal
if ($LASTEXITCODE -ne 0) { throw "build failed" }

if ($Stack) {
    # The published Postgres port is 5433, not 5432: a machine that develops on
    # Postgres usually already has one on the default port, and when they
    # collide every connection lands on the other database and fails
    # authentication while the container still reports healthy.
    $env:TRACE_PORTAL_POSTGRES   = 'postgres://trace:trace@localhost:5433/trace_portal?sslmode=disable'
    $env:TRACE_PORTAL_S3_ENDPOINT = 'localhost:9000'
    $env:TRACE_PORTAL_S3_BUCKET   = 'trace-portal'
    $env:TRACE_PORTAL_S3_KEY      = 'trace'
    $env:TRACE_PORTAL_S3_SECRET   = 'tracetrace'
    $env:TRACE_PORTAL_S3_REGION   = 'us-east-1'

    Write-Host "Checking the stack is up..."
    & docker compose up -d postgres minio | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "docker compose failed -- is Docker Desktop running?" }
} else {
    # Explicitly cleared, so a shell that ran -Stack earlier does not silently
    # keep pointing at a database this invocation did not ask for.
    foreach ($v in 'TRACE_PORTAL_POSTGRES','TRACE_PORTAL_S3_ENDPOINT','TRACE_PORTAL_S3_BUCKET',
                   'TRACE_PORTAL_S3_KEY','TRACE_PORTAL_S3_SECRET','TRACE_PORTAL_S3_REGION') {
        Remove-Item "env:$v" -ErrorAction SilentlyContinue
    }
}

Stop-TracePortal

$log = Join-Path $env:TEMP 'trace-portal.log'
Start-Process -FilePath (Join-Path $root 'trace-portal.exe') `
    -ArgumentList '-data', $Data, '-addr', "127.0.0.1:$Port" `
    -RedirectStandardError $log -RedirectStandardOutput "$log.out" `
    -WindowStyle Hidden

Start-Sleep -Seconds 3
$up = $false
foreach ($i in 1..20) {
    try { Invoke-WebRequest -Uri "http://127.0.0.1:$Port/api/health" -UseBasicParsing -TimeoutSec 5 | Out-Null; $up = $true; break }
    catch { Start-Sleep -Seconds 2 }
}

if (-not $up) {
    Write-Host "Did not come up. Last log lines:" -ForegroundColor Red
    Get-Content $log -Tail 20
    return
}

Write-Host ""
Write-Host "  trace-portal is running: http://127.0.0.1:$Port" -ForegroundColor Green
Write-Host "  storage: $(if ($Stack) { 'postgres + minio' } else { 'local files' })"
Write-Host "  data:    $Data"
Write-Host "  log:     $log"
Write-Host "  stop:    .\scripts\dev.ps1 -Stop"
Write-Host ""
