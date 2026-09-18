[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [ValidateRange(1, 65535)][int]$ApiPort = 2022,
    [ValidateRange(1, 65535)][int]$ProxyPort = 2023,
    [string]$ProxyHost = '127.0.0.1',
    [ValidateRange(0, 120)][int]$GraceSeconds = 15
)

$ErrorActionPreference = 'Stop'
$appExe = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot 'wx_video_download.exe'))
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

function Initialize-ProxyConnectionApi {
    if ('WxChannelsStop.ProxyConnection' -as [type]) { return }
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
namespace WxChannelsStop {
    public static class ProxyConnection {
        [StructLayout(LayoutKind.Explicit)] public struct Value {
            [FieldOffset(0)] public uint Flags;
            [FieldOffset(0)] public IntPtr String;
            [FieldOffset(0)] public System.Runtime.InteropServices.ComTypes.FILETIME Time;
        }
        [StructLayout(LayoutKind.Sequential)] public struct Option { public uint Id; public Value Data; }
        [StructLayout(LayoutKind.Sequential)] public struct OptionList {
            public uint Size; public IntPtr Connection; public uint Count; public uint Error; public IntPtr Options;
        }
        public sealed class Settings { public uint Flags; public string Server; }
        [DllImport("wininet.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool InternetQueryOptionW(IntPtr h, uint option, ref OptionList list, ref uint size);
        [DllImport("wininet.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool InternetSetOptionW(IntPtr h, uint option, IntPtr buffer, uint size);
        [DllImport("kernel32.dll")] private static extern IntPtr GlobalFree(IntPtr p);
        private static Settings Query(uint flagsOption) {
            int stride = Marshal.SizeOf(typeof(Option));
            IntPtr buffer = Marshal.AllocHGlobal(stride * 2);
            try {
                Marshal.StructureToPtr(new Option { Id=flagsOption }, buffer, false);
                Marshal.StructureToPtr(new Option { Id=2 }, IntPtr.Add(buffer, stride), false);
                OptionList list = new OptionList { Size=(uint)Marshal.SizeOf(typeof(OptionList)), Count=2, Options=buffer };
                uint size = list.Size;
                bool ok = InternetQueryOptionW(IntPtr.Zero, 75, ref list, ref size);
                int error = Marshal.GetLastWin32Error();
                Option flags = (Option)Marshal.PtrToStructure(buffer, typeof(Option));
                Option server = (Option)Marshal.PtrToStructure(IntPtr.Add(buffer, stride), typeof(Option));
                try {
                    if (!ok) throw new Win32Exception(error, "Cannot query Windows proxy connection.");
                    return new Settings { Flags=flags.Data.Flags, Server=Marshal.PtrToStringUni(server.Data.String) ?? "" };
                } finally { if (server.Data.String != IntPtr.Zero) GlobalFree(server.Data.String); }
            } finally { Marshal.FreeHGlobal(buffer); }
        }
        public static Settings Read() {
            try { return Query(10); } catch (Win32Exception) { return Query(1); }
        }
        public static void WriteFlags(uint flags) {
            IntPtr option = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(Option)));
            IntPtr buffer = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(OptionList)));
            try {
                Marshal.StructureToPtr(new Option { Id=1, Data=new Value { Flags=flags } }, option, false);
                OptionList list = new OptionList { Size=(uint)Marshal.SizeOf(typeof(OptionList)), Count=1, Options=option };
                Marshal.StructureToPtr(list, buffer, false);
                if (!InternetSetOptionW(IntPtr.Zero, 75, buffer, list.Size))
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "Cannot update Windows proxy connection.");
            } finally { Marshal.FreeHGlobal(buffer); Marshal.FreeHGlobal(option); }
        }
        public static void Refresh() {
            foreach (uint option in new uint[] {95, 39, 37})
                if (!InternetSetOptionW(IntPtr.Zero, option, IntPtr.Zero, 0))
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "Cannot refresh Windows proxy settings.");
        }
    }
}
'@
}

function Get-EffectiveProxySettings { [WxChannelsStop.ProxyConnection]::Read() }
function Set-EffectiveProxyFlags([uint32]$Flags) {
    [WxChannelsStop.ProxyConnection]::WriteFlags($Flags)
}
function Send-ProxySettingsChanged { [WxChannelsStop.ProxyConnection]::Refresh() }

function Get-LegacyProxySettings {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Internet Settings', $false)
    try {
        $names = if ($null -eq $key) { @() } else { @($key.GetValueNames()) }
        $enableExists = $names -contains 'ProxyEnable'
        $serverExists = $names -contains 'ProxyServer'
        if ($enableExists -and $key.GetValueKind('ProxyEnable') -ne [Microsoft.Win32.RegistryValueKind]::DWord) {
            throw 'Unexpected ProxyEnable registry type; preserving the setting.'
        }
        if ($serverExists -and $key.GetValueKind('ProxyServer') -ne [Microsoft.Win32.RegistryValueKind]::String) {
            throw 'Unexpected ProxyServer registry type; preserving the setting.'
        }
        [pscustomobject]@{
            EnableExists = $enableExists
            Enable = $(if ($enableExists) { $key.GetValue('ProxyEnable') } else { 0 })
            ServerExists = $serverExists
            Server = $(if ($serverExists) { [string]$key.GetValue('ProxyServer') } else { '' })
        }
    } finally { if ($null -ne $key) { $key.Dispose() } }
}

function Restore-LegacyProxySettings($Previous) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Internet Settings', $true)
    if ($null -eq $key) {
        if (-not $Previous.EnableExists -and -not $Previous.ServerExists) { return }
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Software\Microsoft\Windows\CurrentVersion\Internet Settings')
    }
    try {
        if ($Previous.ServerExists) { $key.SetValue('ProxyServer', $Previous.Server, [Microsoft.Win32.RegistryValueKind]::String) }
        else { $key.DeleteValue('ProxyServer', $false) }
        if ($Previous.EnableExists) { $key.SetValue('ProxyEnable', $Previous.Enable, [Microsoft.Win32.RegistryValueKind]::DWord) }
        else { $key.DeleteValue('ProxyEnable', $false) }
    } finally { $key.Dispose() }
}

function Test-OwnedProxyAddress([string]$Server) {
    $entries = @($Server.Split(';') | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($entries.Count -eq 0) { return $false }
    foreach ($entry in $entries) {
        if (($entry -split '=', 2)[-1].Trim() -ne $proxyAddress) { return $false }
    }
    return $true
}

function Test-LegacyProxyEqual($Left, $Right) {
    return $Left.EnableExists -eq $Right.EnableExists -and $Left.Enable -eq $Right.Enable -and
        $Left.ServerExists -eq $Right.ServerExists -and $Left.Server -ceq $Right.Server
}

function Disable-OwnedProxyUnlocked {
    # A new instance or another application may now own this endpoint.
    if (@(Get-PortListeners $ProxyPort).Count -gt 0) {
        Write-Step "Port $ProxyPort still has a listener; preserving the proxy setting."
        return
    }
    Initialize-ProxyConnectionApi
    $settings = Get-EffectiveProxySettings
    $legacy = Get-LegacyProxySettings
    $connectionEnabled = ($settings.Flags -band 2) -ne 0
    $legacyEnabled = $legacy.EnableExists -and $legacy.Enable -ne 0
    if (-not $connectionEnabled -and -not $legacyEnabled) { return }
    if (($connectionEnabled -and -not (Test-OwnedProxyAddress $settings.Server)) -or
        ($legacyEnabled -and -not (Test-OwnedProxyAddress $legacy.Server))) {
        Write-Step 'An enabled proxy includes another address; preserving both settings.'
        return
    }
    # External proxy managers do not participate in our mutex. Recheck before writing.
    $fresh = Get-EffectiveProxySettings
    $freshLegacy = Get-LegacyProxySettings
    if ($fresh.Flags -ne $settings.Flags -or $fresh.Server -cne $settings.Server -or
        -not (Test-LegacyProxyEqual $freshLegacy $legacy) -or @(Get-PortListeners $ProxyPort).Count -gt 0) {
        Write-Step 'The proxy changed while stopping; preserving its current setting.'
        return
    }
    $nextFlags = [uint32]($settings.Flags -band (-bnot 2))
    if ($connectionEnabled -and $nextFlags -eq 0) { $nextFlags = 1 }
    $nextLegacy = [pscustomobject]@{
        EnableExists = $legacy.EnableExists
        Enable = 0
        ServerExists = $legacy.ServerExists
        Server = $legacy.Server
    }
    try {
        # Preserve server/bypass/PAC values. Both manual switches must agree,
        # including installations left with DIRECT plus legacy ProxyEnable=1.
        if ($connectionEnabled) { Set-EffectiveProxyFlags $nextFlags }
        # Some Windows versions mirror native flags writes into legacy fields.
        # Keep the original address and field presence, with only enable cleared.
        Restore-LegacyProxySettings $nextLegacy
        Send-ProxySettingsChanged
        $after = Get-EffectiveProxySettings
        $afterLegacy = Get-LegacyProxySettings
        if ($after.Flags -ne $nextFlags -or $after.Server -cne $settings.Server -or
            -not (Test-LegacyProxyEqual $afterLegacy $nextLegacy)) {
            throw 'Proxy settings did not converge after shutdown.'
        }
    } catch {
        $failure = $_
        $rollbackErrors = New-Object 'System.Collections.Generic.List[System.Exception]'
        $rollbackErrors.Add($failure.Exception)
        # Native flags writes may mirror legacy values; restore legacy last so
        # even a pre-existing mismatch or absent registry value is preserved.
        try { Set-EffectiveProxyFlags $settings.Flags } catch { $rollbackErrors.Add($_.Exception) }
        try { Restore-LegacyProxySettings $legacy } catch { $rollbackErrors.Add($_.Exception) }
        try { Send-ProxySettingsChanged } catch { $rollbackErrors.Add($_.Exception) }
        try {
            $restored = Get-EffectiveProxySettings
            $restoredLegacy = Get-LegacyProxySettings
            if ($restored.Flags -ne $settings.Flags -or $restored.Server -cne $settings.Server -or
                -not (Test-LegacyProxyEqual $restoredLegacy $legacy)) {
                throw 'Proxy rollback did not restore the original settings.'
            }
        } catch { $rollbackErrors.Add($_.Exception) }
        if ($rollbackErrors.Count -gt 1) { throw [System.AggregateException]::new('Proxy shutdown and rollback failed.', $rollbackErrors.ToArray()) }
        throw $failure
    }
    Write-Step "Disabled the inactive application proxy $proxyAddress."
}

if (-not $PSCmdlet.ShouldProcess($appExe, 'Stop this installation and restore its inactive system proxy')) { return }

Write-Step 'Pausing active downloads and requesting graceful shutdown...'
Invoke-OwnedApi '/api/v1/download_task/pause_all' '{"status":"running"}'
Invoke-OwnedApi '/api/service/stop?name=application'

$deadline = (Get-Date).AddSeconds($GraceSeconds)
while (@(Get-AppProcesses).Count -gt 0 -and (Get-Date) -lt $deadline) {
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
