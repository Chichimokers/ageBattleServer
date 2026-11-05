<#
.SYNOPSIS
    Runs cmd/loadtest against a running battleserver and records the machine load
    with every figure it prints.

.DESCRIPTION
    cmd/loadtest reports what the GENERATOR did: its own CPU, its own heap, and the
    traffic it exchanged. Its `cpu=` line is this process's, not the server's, and a
    figure that says "the load test used 40 % of a core" says nothing about the
    server under it.

    So this script measures the two things the tool cannot:

      * the SERVER's CPU, read from its own process (TotalProcessorTime), over the
        same wall clock the load ran for;
      * the MACHINE's busy fraction, from Win32_Processor.LoadPercentage, before and
        after. PERFORMANCE.md 1 records the load beside every clock figure because a
        figure without its load is comparable to nothing, and a load test is the
        worst case: the generator and the server share one machine, so a busy
        machine is a slower server and a slower generator at once.

    The CPU percentage is divided by the logical processor count, so 100 % means
    one whole core and not one whole machine. That is the only reading that can be
    compared between machines with different core counts.

    Nothing here samples memory through WMI. PERFORMANCE.md 2 records three separate
    ways a PowerShell sampler reported 0 MB while sampling correctly, and every one
    of them was a shape problem in the shell layer. The memory figures come from
    cmd/benchpeer -load, which reads them with the Win32 process APIs directly.

.PARAMETER Server
    The WebSocket endpoint of the running server, e.g. ws://127.0.0.1:14003/.

.PARAMETER ServerPid
    The server process id. Its CPU is what this script reports; without it there is
    no server figure and the script says so instead of printing the generator's.

.PARAMETER Matches
    Concurrent sessions. The plan's figure is 4.

.PARAMETER Players
    Players per session, including the host. The plan's figure is 8.

.PARAMETER Observers
    Observers per session. The plan's figure is 24.

.PARAMETER Duration
    How long the traffic runs, as a Go duration string.

.PARAMETER Loadtest
    Path to loadtest.exe. Defaults to .\loadtest.exe.

.EXAMPLE
    .\scripts\loadtest.ps1 -Server ws://127.0.0.1:14003/ -ServerPid 12345 `
        -Matches 4 -Players 8 -Observers 24 -Duration 60s
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [Parameter(Mandatory = $true)][int]$ServerPid,
    [int]$Matches = 4,
    [int]$Players = 8,
    [int]$Observers = 24,
    [string]$Duration = '60s',
    [string]$Loadtest = '.\loadtest.exe'
)

$ErrorActionPreference = 'Stop'

function Get-MachineLoadPercent {
    # LoadPercentage is per logical processor, so the average is the machine's busy
    # fraction. A single processor reporting 100 % is one core pinned, not a busy
    # machine, and reporting the maximum would say the opposite.
    $cpus = Get-CimInstance -ClassName Win32_Processor
    if (-not $cpus) {
        return $null
    }
    $sum = 0
    $n = 0
    foreach ($c in $cpus) {
        if ($null -ne $c.LoadPercentage) {
            $sum += [int]$c.LoadPercentage
            $n++
        }
    }
    if ($n -eq 0) {
        return $null
    }
    return [math]::Round($sum / $n, 1)
}

function Get-ProcessCpuSeconds {
    param([int]$Id)
    $p = Get-Process -Id $Id -ErrorAction SilentlyContinue
    if (-not $p) {
        return $null
    }
    return $p.TotalProcessorTime.TotalSeconds
}

if (-not (Test-Path -LiteralPath $Loadtest)) {
    throw "$Loadtest not found. Build it with: go build -o loadtest.exe ./cmd/loadtest"
}

$server = Get-Process -Id $ServerPid -ErrorAction SilentlyContinue
if (-not $server) {
    throw "no process with id ${ServerPid}: the server's CPU is the figure this script exists for, and there is nothing to read it from"
}

$logical = [Environment]::ProcessorCount
$loadBefore = Get-MachineLoadPercent
$cpuBefore = Get-ProcessCpuSeconds -Id $ServerPid
if ($null -eq $cpuBefore) {
    throw "could not read the CPU time of process $ServerPid"
}

$clock = [System.Diagnostics.Stopwatch]::StartNew()
$output = & $Loadtest -server $Server -matches $Matches -players $Players -observers $Observers -duration $Duration 2>&1
$exit = $LASTEXITCODE
$clock.Stop()

$cpuAfter = Get-ProcessCpuSeconds -Id $ServerPid
$loadAfter = Get-MachineLoadPercent

$wall = $clock.Elapsed.TotalSeconds
if ($wall -le 0) {
    $wall = 1
}

Write-Output $output

Write-Output ''
Write-Output '=== machine load ==='
Write-Output ("logical processors            : {0}" -f $logical)
Write-Output ("wall clock                    : {0:N2} s" -f $wall)
if ($null -eq $cpuAfter) {
    Write-Output 'server CPU                    : n/a (the process exited during the run)'
} else {
    $used = $cpuAfter - $cpuBefore
    $pct = 100 * $used / $wall / $logical
    Write-Output ("server CPU                    : {0:N2} s over {1:N2} s = {2:N2} % of the machine ({3:N2} % of one core)" -f $used, $wall, $pct, ($pct * $logical))
}
if ($null -eq $loadBefore -or $null -eq $loadAfter) {
    Write-Output 'Win32_Processor.LoadPercentage: n/a (WMI refused the query)'
} else {
    Write-Output ("Win32_Processor.LoadPercentage: {0:N1} % before, {1:N1} % after" -f $loadBefore, $loadAfter)
}
Write-Output ''
Write-Output 'The `cpu=` line above is the GENERATOR''s, not the server''s: the two share this'
Write-Output 'machine, so read the server figure beside the machine load and never alone.'

if ($exit -ne 0) {
    exit $exit
}
