# Portable LLM launcher - Windows (x86_64, CPU build)
$ErrorActionPreference = 'Stop'
$Dir    = $PSScriptRoot
$Bin    = Join-Path $Dir 'bin\win-x64\llama-server.exe'
$Models = Join-Path $Dir 'models'
$Ui     = Join-Path $Dir 'ui'
$Ctx    = 8192

Clear-Host
Write-Host "  Portable LLM"
Write-Host "  ----------------------------------------"

if (-not (Test-Path $Bin)) {
    Write-Host "  x Missing binary: $Bin" -ForegroundColor Red
    Read-Host "  Press Enter to close"; exit 1
}

# --- pick model by available RAM ---------------------------------------
$Ram    = (Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
$RamGb  = [math]::Round($Ram / 1GB)
# Budget = min(70% of RAM, RAM - 4GB) -- see run-mac.command for rationale.
$Budget = [Math]::Min($Ram * 0.7, $Ram - 4GB)

$Best = Get-ChildItem -Path $Models -Filter *.gguf -ErrorAction SilentlyContinue |
        Where-Object { $_.Length -le $Budget } |
        Sort-Object Length -Descending | Select-Object -First 1

if (-not $Best) {
    Write-Host "  x No model fits in ${RamGb}GB of RAM." -ForegroundColor Red
    Get-ChildItem -Path $Models -Filter *.gguf -ErrorAction SilentlyContinue |
        ForEach-Object { Write-Host ("      {0}  {1:N1}GB" -f $_.Name, ($_.Length/1GB)) }
    Read-Host "  Press Enter to close"; exit 1
}

# --- find a free port --------------------------------------------------
$Port = 8080
while (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) { $Port++ }

$Cores = (Get-CimInstance Win32_ComputerSystem).NumberOfLogicalProcessors
Write-Host "  Machine : ${RamGb}GB RAM - CPU inference"
Write-Host ("  Model   : {0} ({1:N1}GB)" -f $Best.Name, ($Best.Length/1GB))
Write-Host "  Address : http://127.0.0.1:$Port"
Write-Host "  ----------------------------------------"
Write-Host "  Loading... close this window to shut down."
Write-Host ""

$logPath = Join-Path $Dir 'logs\server.log'
$proc = Start-Process -FilePath $Bin -PassThru -NoNewWindow -RedirectStandardOutput $logPath `
    -RedirectStandardError (Join-Path $Dir 'logs\server.err.log') `
    -ArgumentList @('-m', $Best.FullName, '--host','127.0.0.1', '--port', $Port,
                    '--path', $Ui, '-c', $Ctx, '-t', $Cores, '--no-warmup', '--cors-origins','localhost')

try {
    foreach ($i in 1..600) {
        if ($proc.HasExited) {
            Write-Host "  x Server exited." -ForegroundColor Red
            Get-Content $logPath -Tail 15 -ErrorAction SilentlyContinue
            Read-Host "  Press Enter to close"; exit 1
        }
        try {
            Invoke-WebRequest "http://127.0.0.1:$Port/health" -UseBasicParsing -TimeoutSec 2 | Out-Null
            Write-Host "  + Ready - opening browser." -ForegroundColor Green
            Start-Process "http://127.0.0.1:$Port"
            break
        } catch { Start-Sleep -Seconds 1 }
    }
    $proc.WaitForExit()
} finally {
    if (-not $proc.HasExited) { $proc.Kill() }
}
