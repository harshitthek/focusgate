param(
    [string]$BaseUrl = "http://127.0.0.1:8787"
)

$ErrorActionPreference = "Stop"
$health = Invoke-RestMethod -Uri "$BaseUrl/api/health"
$headers = @{}
if ($env:FOCUSGATE_AUTH_TOKEN) {
    $headers["Authorization"] = "Bearer $($env:FOCUSGATE_AUTH_TOKEN)"
}
$status = Invoke-RestMethod -Uri "$BaseUrl/api/status" -Headers $headers
$proc = Get-Process -Name focusgate -ErrorAction SilentlyContinue | Select-Object -First 1

Write-Host "Health OK: $($health.ok)"
Write-Host "Mode: $($status.stats.mode) | Queued: $($status.stats.queued_count) | Pushed today: $($status.stats.pushed_today)"
Write-Host "Classifier: $($status.classifier)"
Write-Host ("Go Heap: {0:N2} MB | DB: {1:N2} KB" -f $status.heap_mb, $status.db_size_kb)
if ($proc) {
    Write-Host ("Process WorkingSet64: {0:N2} MB (PID {1})" -f ($proc.WorkingSet64 / 1MB), $proc.Id)
}
