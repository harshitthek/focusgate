$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RootDir "dist"
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Out = "focusgate-windows-amd64.exe" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Out = "focusgate-linux-amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Out = "focusgate-linux-arm64" }
)

Push-Location $RootDir
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
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
}
