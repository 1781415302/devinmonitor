# DevinMonitor tray: silent background start + notify-area icon.
# Keep this file ASCII-only for Windows PowerShell 5.1.

param(
    [int]$Port = 19191,
    [string]$AppDir = $PSScriptRoot
)

$ErrorActionPreference = 'Continue'
$exe = Join-Path $AppDir 'devinmonitor.exe'
$url = "http://127.0.0.1:$Port/"

if (-not (Test-Path $exe)) { exit 1 }

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

# Single instance: second double-click just opens the dashboard.
$mutex = $null
try {
    $mutex = New-Object System.Threading.Mutex($false, 'Global\DevinMonitorTray')
    if (-not $mutex.WaitOne(0)) {
        try { Start-Process $url } catch {}
        $mutex.Dispose()
        exit 0
    }
} catch {
    # Mutex unavailable — continue without single-instance guard.
}

function Test-PortReady {
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $c.Connect('127.0.0.1', $Port)
        $c.Close()
        return $true
    } catch {
        return $false
    }
}

function Stop-Server {
    Get-Process -Name devinmonitor -ErrorAction SilentlyContinue | ForEach-Object {
        try { $_.Kill() } catch {}
    }
}

function Start-Server {
    Stop-Server
    Start-Sleep -Milliseconds 400
    Start-Process -FilePath $exe -ArgumentList @('web','--port',"$Port") -WorkingDirectory $AppDir -WindowStyle Hidden
}

$icon = New-Object System.Windows.Forms.NotifyIcon
$icon.Icon = [System.Drawing.SystemIcons]::Application
$icon.Text = 'DevinMonitor'
$icon.Visible = $true

$menu = New-Object System.Windows.Forms.ContextMenuStrip

$openItem = New-Object System.Windows.Forms.ToolStripMenuItem
$openItem.Text = 'Open dashboard'
$openItem.Add_Click({ Start-Process $url })

$restartItem = New-Object System.Windows.Forms.ToolStripMenuItem
$restartItem.Text = 'Restart service'
$restartItem.Add_Click({
    Start-Server
    $ok = $false
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Milliseconds 500
        if (Test-PortReady) { $ok = $true; break }
    }
    if ($ok) { $script:icon.Text = 'DevinMonitor (running)' }
    else { $script:icon.Text = 'DevinMonitor (failed)' }
})

$exitItem = New-Object System.Windows.Forms.ToolStripMenuItem
$exitItem.Text = 'Exit'
$exitItem.Add_Click({
    $script:icon.Visible = $false
    Stop-Server
    try { if ($script:mutex) { $script:mutex.ReleaseMutex(); $script:mutex.Dispose() } } catch {}
    [System.Windows.Forms.Application]::Exit()
})

$menu.Items.Add($openItem) | Out-Null
$menu.Items.Add($restartItem) | Out-Null
$menu.Items.Add((New-Object System.Windows.Forms.ToolStripSeparator)) | Out-Null
$menu.Items.Add($exitItem) | Out-Null
$icon.ContextMenuStrip = $menu
$icon.Add_DoubleClick({ Start-Process $url })

if (-not (Test-PortReady)) {
    Start-Server
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Milliseconds 500
        if (Test-PortReady) { break }
    }
}

if (Test-PortReady) {
    $icon.Text = 'DevinMonitor (running)'
    try { $icon.ShowBalloonTip(2000, 'DevinMonitor', 'Running in background') } catch {}
    Start-Process $url
} else {
    $icon.Text = 'DevinMonitor (failed)'
}

[System.Windows.Forms.Application]::Run()

Stop-Server
try { $icon.Visible = $false; $icon.Dispose() } catch {}
try { if ($mutex) { $mutex.ReleaseMutex(); $mutex.Dispose() } } catch {}
