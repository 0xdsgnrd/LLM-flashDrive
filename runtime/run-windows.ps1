# Pocket LLM launcher - Windows (x86_64, CPU build), router mode.
$ErrorActionPreference = 'Stop'
$Dir    = $PSScriptRoot
$Bin    = Join-Path $Dir 'bin\win-x64\llama-server.exe'
$Models = Join-Path $Dir 'models'
$Ui     = Join-Path $Dir 'ui'
$Ctx    = 8192

Clear-Host
Write-Host "  Pocket LLM"
Write-Host "  ----------------------------------------"

if (-not (Test-Path $Bin)) {
    Write-Host "  x Missing binary: $Bin" -ForegroundColor Red
    Read-Host "  Press Enter to close"; exit 1
}

$Ram    = (Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
$RamGb  = [math]::Round($Ram / 1GB)
$Budget = [Math]::Min($Ram * 0.7, $Ram - 4GB)

# See run-mac.command: multi-part models are one model across several files and
# nothing groups them, so counting each part separately breaks the size math.
$AllFiles = @(Get-ChildItem -Path $Models -Filter *.gguf -ErrorAction SilentlyContinue)
$Split = @($AllFiles | Where-Object { $_.Name -match '-\d{5}-of-\d{5}\.gguf$' })
$Files = @($AllFiles | Where-Object { $_.Name -notmatch '-\d{5}-of-\d{5}\.gguf$' })
if ($Split.Count -gt 0) {
    Write-Host "  ! Skipping $($Split.Count) multi-part file(s) - split models are not supported yet."
}
if ($Files.Count -eq 0) {
    Write-Host "  x No usable .gguf files in $Models" -ForegroundColor Red
    Read-Host "  Press Enter to close"; exit 1
}

# See run-mac.command: --models-max default of 4 is fatal on small machines.
# Worst case is the N largest all resident, so size N against real files.
$Fitting = 0; $Sum = 0; $MaxN = 0; $Packing = $true
foreach ($f in ($Files | Sort-Object Length -Descending)) {
    if ($f.Length -gt $Budget) { continue }
    $Fitting++
    # Stop growing MaxN at the first model that does not fit: --models-max is a
    # COUNT, not a set, so the only safe N is one where the N LARGEST fit
    # together. Keep looping so $Fitting still counts the rest.
    if ($Packing -and (($Sum + $f.Length) -le $Budget)) { $Sum += $f.Length; $MaxN++ }
    else { $Packing = $false }
}
if ($Fitting -eq 0) {
    Write-Host "  x No model fits in ${RamGb}GB of RAM." -ForegroundColor Red
    $Files | ForEach-Object { Write-Host ("      {0}  {1:N1}GB" -f $_.Name, ($_.Length/1GB)) }
    Read-Host "  Press Enter to close"; exit 1
}
if ($MaxN -lt 1) { $MaxN = 1 }

# Manifest so the UI can grey out models this machine cannot run.
try {
    $entries = $Files | ForEach-Object {
        $n = [System.IO.Path]::GetFileNameWithoutExtension($_.Name)
        $fits = if ($_.Length -le $Budget) { 'true' } else { 'false' }
        '"{0}":{{"bytes":{1},"fits":{2}}}' -f $n, $_.Length, $fits
    }
    $json = '{{"ramBytes":{0},"budgetBytes":{1},"modelsMax":{2},"models":{{{3}}}}}' -f `
            $Ram, [int64]$Budget, $MaxN, ($entries -join ',')
    Set-Content -Path (Join-Path $Ui 'machine.json') -Value $json -Encoding ASCII
} catch {
    Write-Host "  (note: manifest not writable - UI will offer all models)"
}

$Port = 8080
while (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) { $Port++ }

$Cores = (Get-CimInstance Win32_ComputerSystem).NumberOfLogicalProcessors
Write-Host "  Machine : ${RamGb}GB RAM - CPU inference"
Write-Host "  Models  : $Fitting of $($Files.Count) usable, up to $MaxN loaded at once"
Write-Host "  Address : http://127.0.0.1:$Port"
Write-Host "  ----------------------------------------"
Write-Host "  Pick a model in the sidebar. Close this window to shut down."
Write-Host ""

$logPath = Join-Path $Dir 'logs\server.log'
$proc = Start-Process -FilePath $Bin -PassThru -NoNewWindow -RedirectStandardOutput $logPath `
    -RedirectStandardError (Join-Path $Dir 'logs\server.err.log') `
    -ArgumentList @('--models-dir', $Models, '--host','127.0.0.1', '--port', $Port,
                    '--path', $Ui, '-c', $Ctx, '-t', $Cores,
                    '--models-max', $MaxN, '--cors-origins','localhost','--no-warmup')

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
