[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [ValidateRange(1, 65535)][int]$ApiPort = 2022,
    [ValidateRange(1, 65535)][int]$ProxyPort = 2023,
    [string]$ProxyHost = '127.0.0.1',
    [ValidateRange(0, 120)][int]$GraceSeconds = 15
)

$ErrorActionPreference = 'Stop'
$appExe = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot 'wx_video_download.exe'))
$registryPath = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings'
$proxyAddress = if ($ProxyHost.Contains(':')) { "[$ProxyHost]:$ProxyPort" } else { "${ProxyHost}:$ProxyPort" }

function Write-Step([string]$Message) { Write-Host "[wx_channels_download] $Message" }

function Get-AppProcesses {
    $candidates = @(Get-CimInstance Win32_Process -Filter "Name='wx_video_download.exe'")
    if ($candidates | Where-Object { -not $_.ExecutablePath }) {
        throw 'Cannot verify the executable path of a downloader process. Run this script with the privileges used to start the application.'
    }
    @($candidates | Where-Object {
        $_.ExecutablePath -and [System.IO.Path]::GetFullPath($_.ExecutablePath) -eq $appExe
    })
}

function Get-PortListeners([int]$Port) {
    @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

function Invoke-OwnedApi([string]$Path, [string]$Body = '{}') {
    $appIds = @(Get-AppProcesses | ForEach-Object { [int]$_.ProcessId })
    $listeners = @(Get-PortListeners $ApiPort)
    if (-not ($listeners | Where-Object { $appIds -contains [int]$_.OwningProcess })) {
        Write-Step "API port $ApiPort is not owned by this installation; skipping $Path."
        return
    }
    try {
        Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$ApiPort$Path" `
            -ContentType 'application/json' -Body $Body -TimeoutSec 20 | Out-Null
    } catch {
        Write-Step "API request failed: $($_.Exception.Message)"
    }
}

function Disable-OwnedProxy {
    # Use the same user-scoped mutex as the Go guardian/EnableProxy path.
    $ownerPath = Join-Path $env:LOCALAPPDATA 'wx_channels_download\proxy-owner'
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { $hash = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($ownerPath.ToLowerInvariant())) }
    finally { $sha.Dispose() }
    $name = 'Local\wx_channels_download_proxy_' + (-join ($hash[0..15] | ForEach-Object { $_.ToString('x2') }))
    $mutex = New-Object System.Threading.Mutex($false, $name)
    $locked = $false
    try {
        try { $locked = $mutex.WaitOne(30000) }
        catch [System.Threading.AbandonedMutexException] { $locked = $true }
        if (-not $locked) { throw 'Timed out waiting for the application proxy lock.' }
        Disable-OwnedProxyUnlocked
    } finally {
        if ($locked) { $mutex.ReleaseMutex() }
        $mutex.Dispose()
    }
}

function Disable-OwnedProxyUnlocked {
    # A new instance or another application may now own this endpoint.
    if ((Get-PortListeners $ProxyPort).Count -gt 0) {
        Write-Step "Port $ProxyPort still has a listener; preserving the proxy setting."
        return
    }
    $settings = Get-ItemProperty -LiteralPath $registryPath
    if ([int]$settings.ProxyEnable -ne 1) { return }
    $entries = @(([string]$settings.ProxyServer).Split(';') | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($entries.Count -eq 0) { return }
    foreach ($entry in $entries) {
        $address = ($entry -split '=', 2)[-1].Trim()
        if ($address -ne $proxyAddress) {
            Write-Step 'The current proxy includes another address; preserving it.'
            return
        }
    }
    Set-ItemProperty -LiteralPath $registryPath -Name ProxyEnable -Type DWord -Value 0
    if (-not ('WxChannelsStop.WinInet' -as [type])) {
        Add-Type -Namespace WxChannelsStop -Name WinInet -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("wininet.dll", SetLastError=true)]
public static extern bool InternetSetOption(System.IntPtr hInternet, int option, System.IntPtr buffer, int length);
'@
    }
    foreach ($option in @(39, 37)) {
        if (-not [WxChannelsStop.WinInet]::InternetSetOption([IntPtr]::Zero, $option, [IntPtr]::Zero, 0)) {
            throw 'Windows rejected the proxy settings refresh.'
        }
    }
    Write-Step "Disabled the inactive application proxy $proxyAddress."
}

if (-not $PSCmdlet.ShouldProcess($appExe, 'Stop this installation and restore its inactive system proxy')) { return }

Write-Step 'Pausing active downloads and requesting graceful shutdown...'
Invoke-OwnedApi '/api/v1/download_task/pause_all' '{"status":"running"}'
Invoke-OwnedApi '/api/service/stop?name=application'

$deadline = (Get-Date).AddSeconds($GraceSeconds)
while ((Get-AppProcesses).Count -gt 0 -and (Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 250
}
foreach ($target in (Get-AppProcesses)) {
    # Recheck the full path immediately before terminating a PID.
    $current = Get-CimInstance Win32_Process -Filter "ProcessId=$($target.ProcessId)"
    if ($current.ExecutablePath -and [System.IO.Path]::GetFullPath($current.ExecutablePath) -eq $appExe) {
        Write-Step "Stopping remaining process $($target.ProcessId)."
        Stop-Process -Id $target.ProcessId -Force -ErrorAction SilentlyContinue
    }
}
Start-Sleep -Milliseconds 500
Disable-OwnedProxy
$remaining = @(Get-AppProcesses)
if ($remaining.Count -gt 0) { throw 'Application processes remain; retry with the same privileges used to start it.' }
Write-Step 'Shutdown complete for this installation.'
