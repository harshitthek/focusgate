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
if (-not (Test-Path $ExePath)) {
    $DistExe = Join-Path $RootDir "dist\focusgate-windows-amd64.exe"
    if (Test-Path $DistExe) {
        $ExePath = $DistExe
    }
}
$CfgPath = Join-Path $RootDir "focusgate.yaml"

if ($Uninstall) {
    try {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
    } catch {
        if ($_.CategoryInfo.Category -ne 'ObjectNotFound' -and $_.Exception.Message -notmatch 'No MSFT_ScheduledTask objects found') {
            throw "Failed to remove scheduled task '$TaskName': $($_.Exception.Message)"
        }
    }

    try {
        Remove-NetFirewallRule -DisplayName "FocusGate LAN (TCP 8787)" -ErrorAction Stop
    } catch {
        if ($_.CategoryInfo.Category -ne 'ObjectNotFound' -and $_.Exception.Message -notmatch 'No MSFT_NetFirewallRule objects found') {
            throw "Failed to remove firewall rule 'FocusGate LAN (TCP 8787)': $($_.Exception.Message)"
        }
    }

    Write-Host "Removed scheduled task '$TaskName' and firewall rule (if present)."
    exit 0
}

if (-not (Test-Path $ExePath)) {
    throw "Executable not found at $ExePath or dist\focusgate-windows-amd64.exe. Build it first with: go build -ldflags=`"-s -w`" -o focusgate.exe ./cmd/focusgate"
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
    -Execute "powershell.exe" `
    -Argument "-NoProfile -WindowStyle Hidden -Command `"`$p = Start-Process -FilePath '$ExePath' -ArgumentList 'serve --config `\`"$CfgPath`\`"' -NoNewWindow -PassThru -Wait; exit `$p.ExitCode`"" `
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
        -Program $ExePath `
        -RemoteAddress LocalSubnet `
        -Profile Private `
        -Action Allow `
        -ErrorAction Stop | Out-Null
    Write-Host "Opened TCP 8787 on Private network profile for phone PWA access."
}
