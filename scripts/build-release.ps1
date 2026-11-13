$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RootDir "dist"
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Out = "focusgate-windows-amd64.exe" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Out = "focusgate-linux-amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Out = "focusgate-linux-arm64" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; Out = "focusgate-darwin-amd64" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Out = "focusgate-darwin-arm64" },
    @{ GOOS = "android"; GOARCH = "arm64"; Out = "focusgate-android-arm64" }
)

Push-Location $RootDir
$origGOOS = [Environment]::GetEnvironmentVariable("GOOS", "Process")
$origGOARCH = [Environment]::GetEnvironmentVariable("GOARCH", "Process")
$origCGO = [Environment]::GetEnvironmentVariable("CGO_ENABLED", "Process")

try {
    $env:CGO_ENABLED = "0"
    $version = try { (git describe --tags --exact-match 2>$null).Trim() } catch { "1.0.0" }
    if ([string]::IsNullOrWhiteSpace($version) -or $version -notmatch '^\d+\.\d+\.\d+' -and $version -notmatch '^v\d+\.\d+\.\d+') { $version = "1.0.0" }
    $commit = try { (git rev-parse --short HEAD 2>$null).Trim() } catch { "dev" }
    if ([string]::IsNullOrWhiteSpace($commit)) { $commit = "dev" }
    $buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ldflags = "-s -w -X focusgate/internal/version.Version=$version -X focusgate/internal/version.Commit=$commit -X focusgate/internal/version.Date=$buildDate"

    foreach ($t in $targets) {
        $env:GOOS = $t.GOOS
        $env:GOARCH = $t.GOARCH
        $outPath = Join-Path $DistDir $t.Out
        Write-Host "Building $($t.GOOS)/$($t.GOARCH) -> $outPath"
        go build -trimpath -ldflags="$ldflags" -o $outPath ./cmd/focusgate
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
