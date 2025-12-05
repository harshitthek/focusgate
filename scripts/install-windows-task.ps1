<#
.SYNOPSIS
Registers (or removes) a Windows Task Scheduler job to run FocusGate on login
and optionally opens port 8787 on Private networks for the Redmi Note 11 5G PWA.
#>
param(
    [switch]$Uninstall,
    [switch]$AllowFirewallLAN
)

$ErrorActionPreference = "Stop"
$TaskName = "FocusGateDaemon"
$RootDir = Split-Path -Parent $PSScriptRoot
$ExePath = Join-Path $RootDir "focusgate.exe"
$CfgPath = Join-Path $RootDir "focusgate.yaml"

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Remove-NetFirewallRule -DisplayName "FocusGate LAN (TCP 8787)" -ErrorAction SilentlyContinue
    Write-Host "Removed scheduled task '$TaskName' and firewall rule (if present)."
    exit 0
}

if (-not (Test-Path $ExePath)) {
    throw "focusgate.exe not found at $ExePath. Build it first with: go build -ldflags=`"-s -w`" -o focusgate.exe ./cmd/focusgate"
}
if (-not (Test-Path $CfgPath)) {
    Copy-Item (Join-Path $RootDir "focusgate.example.yaml") $CfgPath
    Write-Host "Created $CfgPath from focusgate.example.yaml"
}

foreach ($v in 'FOCUSGATE_AUTH_TOKEN', 'FOCUSGATE_WEBHOOK_SECRET', 'TYPESAFE_API_KEY') {
    if (-not [Environment]::GetEnvironmentVariable($v, 'User')) {
        Write-Warning "$v is not set as a persistent User environment variable; the scheduled task will not inherit session-only `$env: values."
    }
}

$Action = New-ScheduledTaskAction `
    -Execute "conhost.exe" `
    -Argument "--headless `"$ExePath`" serve --config `"$CfgPath`"" `
    -WorkingDirectory $RootDir
$Trigger = New-ScheduledTaskTrigger -AtLogOn
$Settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1)

Register-ScheduledTask `
    -TaskName $TaskName `
    -Action $Action `
    -Trigger $Trigger `
    -Settings $Settings `
    -Description "Low-RAM focus-aware notification and feed router" `
    -Force | Out-Null

Write-Host "Registered scheduled task '$TaskName' (runs hidden at user logon)."

if ($AllowFirewallLAN) {
    New-NetFirewallRule `
        -DisplayName "FocusGate LAN (TCP 8787)" `
        -Direction Inbound `
        -Protocol TCP `
        -LocalPort 8787 `
        -Profile Private `
        -Action Allow `
        -ErrorAction Stop | Out-Null
    Write-Host "Opened TCP 8787 on Private network profile for phone PWA access."
}
