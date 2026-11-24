$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install-windows-task.ps1'
$root = Split-Path -Parent $PSScriptRoot
$configPath = Join-Path $root 'focusgate.yaml'
$registeredExe = Join-Path (Join-Path $root 'dist') 'focusgate-windows-amd64.exe'

function Get-ScheduledTask {
    [CmdletBinding()] param($TaskName)
    [pscustomobject]@{ Actions = @([pscustomobject]@{
        Execute = 'powershell.exe'
        Arguments = "-Command Start-Process -FilePath '$($registeredExe.Replace("'", "''"))'"
    }) }
}
function Stop-ScheduledTask { [CmdletBinding()] param($InputObject) }
function Unregister-ScheduledTask {
    [CmdletBinding(SupportsShouldProcess)] param($TaskName)
    $global:fgTestUnregistered = $true
}
function Remove-NetFirewallRule { [CmdletBinding()] param($DisplayName) }
function Get-CimInstance {
    [CmdletBinding()] param($ClassName, $Filter)
    if ($Filter -like 'Name = *') {
        if ($Filter -ne "Name = 'focusgate-windows-amd64.exe'") { throw 'Wrong registered executable filter' }
        $suffix = if ($global:fgTestScenario -eq 'other-config') { '.backup' } else { '' }
        return [pscustomobject]@{
            ProcessId = 42
            CreationDate = [datetime]'2026-01-01'
            ExecutablePath = $registeredExe
            CommandLine = '"' + $registeredExe + '" serve --config "' + $configPath + $suffix + '"'
        }
    }
    $global:fgTestPidQueries++
    if ($global:fgTestScenario -eq 'cim-error') { Write-Error 'CIM unavailable'; return }
    if ($global:fgTestScenario -eq 'cim-not-found-error') { Write-Error 'CIM unavailable' -Category ObjectNotFound; return }
    if ($global:fgTestScenario -eq 'pid-reused') {
        return [pscustomobject]@{ CreationDate = [datetime]'2026-01-02' }
    }
}

foreach ($case in @('dist', 'other-config', 'cim-error', 'cim-not-found-error', 'pid-reused')) {
    $global:fgTestScenario = $case
    $global:fgTestUnregistered = $false
    $global:fgTestPidQueries = 0
    $failed = $false
    try { & $installer -Uninstall } catch {
        $failed = $true
        if ($case -notin @('cim-error', 'cim-not-found-error') -or $_ -notmatch 'CIM unavailable') { throw }
    }
    if ($case -in @('cim-error', 'cim-not-found-error')) {
        if (-not $failed -or $global:fgTestUnregistered) { throw 'CIM failure treated as process exit' }
    } else {
        if ($failed -or -not $global:fgTestUnregistered) { throw "Uninstall failed for $case" }
        if ($case -eq 'other-config' -and $global:fgTestPidQueries -ne 0) { throw 'Config prefix matched unrelated process' }
        if ($case -ne 'other-config' -and $global:fgTestPidQueries -ne 1) { throw 'Registered process exit was not verified' }
    }
}
Write-Host 'PASS: registered dist executable, exact config argument, CIM failure, and PID reuse'
