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

# pocketd takes the public port and llama-server moves to a private one behind
# it, so the browser still sees a single origin and pocketd can own /api/* to
# write chats to the drive. Without the helper, llama-server serves the UI as
# it always did and history is off.
function Get-FreePort([int]$Start) {
    $p = $Start
    while (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue) { $p++ }
    return $p
}

$Port      = Get-FreePort 8080
$Helper    = Join-Path $Dir 'bin\win-x64\pocketd.exe'
$Chats     = Join-Path $Dir 'chats'
$HasHelper = Test-Path $Helper

if ($HasHelper) {
    $LlamaPort = Get-FreePort ($Port + 1)
    $Hist = "on - saved to chats\ on the drive"
} else {
    $LlamaPort = $Port
    $Hist = "OFF - helper missing, nothing will be saved"
}

$Cores = (Get-CimInstance Win32_ComputerSystem).NumberOfLogicalProcessors
Write-Host "  Machine : ${RamGb}GB RAM - CPU inference"
Write-Host "  Models  : $Fitting of $($Files.Count) usable, up to $MaxN loaded at once"
Write-Host "  History : $Hist"
Write-Host "  Address : http://127.0.0.1:$Port"
Write-Host "  ----------------------------------------"
Write-Host "  Pick a model in the sidebar. Close this window to shut down."
Write-Host ""

$llamaArgs = @('--models-dir', $Models, '--host','127.0.0.1', '--port', $LlamaPort,
               '-c', $Ctx, '-t', $Cores, '--models-max', $MaxN,
               '--cors-origins','localhost','--no-warmup')
if (-not $HasHelper) { $llamaArgs += @('--path', $Ui) }

$logPath = Join-Path $Dir 'logs\server.log'
$proc = Start-Process -FilePath $Bin -PassThru -NoNewWindow -RedirectStandardOutput $logPath `
    -RedirectStandardError (Join-Path $Dir 'logs\server.err.log') -ArgumentList $llamaArgs

$helperProc = $null
if ($HasHelper) {
    $helperProc = Start-Process -FilePath $Helper -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $Dir 'logs\pocketd.log') `
        -RedirectStandardError  (Join-Path $Dir 'logs\pocketd.err.log') `
        -ArgumentList @('-port', $Port, '-ui', $Ui, '-chats', $Chats,
                        '-upstream', "127.0.0.1:$LlamaPort")
}

try {
    foreach ($i in 1..600) {
        if ($proc.HasExited) {
            Write-Host "  x Server exited." -ForegroundColor Red
            Get-Content $logPath -Tail 15 -ErrorAction SilentlyContinue
            Read-Host "  Press Enter to close"; exit 1
        }
        if ($helperProc -and $helperProc.HasExited) {
            Write-Host "  x Helper exited." -ForegroundColor Red
            Get-Content (Join-Path $Dir 'logs\pocketd.err.log') -Tail 15 -ErrorAction SilentlyContinue
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
    if ($helperProc -and -not $helperProc.HasExited) { $helperProc.Kill() }
}
