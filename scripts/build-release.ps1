$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RootDir "dist"
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Out = "focusgate-windows-amd64.exe" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Out = "focusgate-linux-amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Out = "focusgate-linux-arm64" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; Out = "focusgate-darwin-amd64" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Out = "focusgate-darwin-arm64" }
)

Push-Location $RootDir
$origGOOS = [Environment]::GetEnvironmentVariable("GOOS", "Process")
$origGOARCH = [Environment]::GetEnvironmentVariable("GOARCH", "Process")
$origCGO = [Environment]::GetEnvironmentVariable("CGO_ENABLED", "Process")

try {
    $env:CGO_ENABLED = "0"
    foreach ($t in $targets) {
        $env:GOOS = $t.GOOS
        $env:GOARCH = $t.GOARCH
        $outPath = Join-Path $DistDir $t.Out
        Write-Host "Building $($t.GOOS)/$($t.GOARCH) -> $outPath"
        go build -trimpath -ldflags="-s -w" -o $outPath ./cmd/focusgate
        if ($LASTEXITCODE -ne 0) {
            throw "Build failed for $($t.GOOS)/$($t.GOARCH)"
        }
    }
    Get-ChildItem $DistDir | Select-Object Name, Length
} finally {
    if ($null -ne $origGOOS) { $env:GOOS = $origGOOS } else { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
    if ($null -ne $origGOARCH) { $env:GOARCH = $origGOARCH } else { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue }
    if ($null -ne $origCGO) { $env:CGO_ENABLED = $origCGO } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }
    Pop-Location
}
