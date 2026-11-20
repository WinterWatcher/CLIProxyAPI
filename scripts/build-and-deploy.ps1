# PowerShell script to build CLIProxyAPI locally via Docker and deploy to server
[CmdletBinding()]
param (
    [switch]$Deploy = $false,
    [string]$ServerHost = "111.229.196.126",
    [string]$ServerKey = "C:\Users\Administrator\.ssh\111.229.196.126_id_ed25519"
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot

Write-Host "===> 1. Local Building CLIProxyAPI (Linux amd64 with CGO) using local Docker..." -ForegroundColor Cyan

$Commit = (git -C $RepoRoot rev-parse --short HEAD).Trim()
$Version = (git -C $RepoRoot describe --tags --always --dirty).Trim()
$BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$OutputDir = Join-Path $RepoRoot "dist"
if (-not (Test-Path $OutputDir)) {
    New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
}

# Run docker build container locally
docker run --rm `
    -e CGO_ENABLED=1 `
    -e GOOS=linux `
    -e GOARCH=amd64 `
    -e GOPROXY="https://goproxy.cn,direct" `
    -v "${RepoRoot}:/app" `
    -w /app `
    golang:1.26.11-bookworm `
    go build -buildvcs=false `
    -ldflags="-s -w -X 'main.Version=$Version' -X 'main.Commit=$Commit' -X 'main.BuildDate=$BuildDate'" `
    -o /app/dist/cli-proxy-api ./cmd/server/

$BinaryPath = Join-Path $OutputDir "cli-proxy-api"
if (-not (Test-Path $BinaryPath)) {
    Write-Error "Build failed: $BinaryPath not found!"
    exit 1
}

$SizeMB = [math]::Round((Get-Item $BinaryPath).Length / 1MB, 2)
Write-Host "===> Build Success! Output: $BinaryPath ($SizeMB MB, Commit: $Commit)" -ForegroundColor Green

if ($Deploy) {
    Write-Host "===> 2. Deploying binary to $ServerHost via scp..." -ForegroundColor Cyan
    scp -i $ServerKey $BinaryPath "root@${ServerHost}:/opt/clirelay-validation/cli-proxy-api.new"
    
    Write-Host "===> 3. Performing atomic swap and restarting clirelay.service..." -ForegroundColor Cyan
    ssh -i $ServerKey "root@${ServerHost}" "mv -f /opt/clirelay-validation/cli-proxy-api.new /opt/clirelay-validation/cli-proxy-api && systemctl restart clirelay && systemctl is-active clirelay"
    
    Write-Host "===> Deployment Completed Successfully!" -ForegroundColor Green
} else {
    Write-Host "Build complete. Pass -Deploy switch to automatically deliver and restart service on server." -ForegroundColor Yellow
}
