# Records the peak memory of the Go test binaries while the suite runs, so the
# 500 MB ceiling in the plan's definition of done is a measurement and not an
# assumption.
#
# The figure is the SUM of the working sets of every test binary alive at the same
# moment, and the peak of that sum. That is the methodology PERFORMANCE.md 2 uses
# and it is the only one that answers the question: `go test ./...` runs one binary
# per package, several of them at once, so the machine's cost is the sum and not
# the largest single process. A maximum over processes would report the biggest
# package and call it the suite.
#
# WorkingSet64 is the current working set, sampled every 500 ms; the peak of the
# sum is what is reported. A spike shorter than the interval can be missed, which
# is why the interval is short and why the figure is reported as a floor rather
# than as an exact maximum.
#
# IT IS NOT FREE. Enumerating every process on the machine twice a second is
# measurable load, and it was enough to flip one verdict: a full run with this
# sampler alongside it failed TestSyntheticCaptureMatchesOriginal, whose fixture is
# a one-second capture and whose ack grouping is decided by whether a 125 ms tick
# lands between two pages. Run it on a suite whose verdicts you are not about to
# trust, or run it alone and take the memory figure from that run.
param([int]$Minutes = 30)

$deadline = (Get-Date).AddMinutes($Minutes)
$peakSum = 0
$peakSumAt = ''
$peakSingle = 0
$peakSingleName = ''
while ((Get-Date) -lt $deadline) {
    $sum = 0
    $names = @()
    # -Name with a wildcard, and not a filter over every process: the OS does the
    # matching, which is what keeps this sampler cheap enough to run beside a suite
    # whose verdicts depend on a 125 ms tick.
    foreach ($p in (Get-Process -Name '*test*' -ErrorAction SilentlyContinue)) {
        try {
            $ws = $p.WorkingSet64
        } catch {
            continue
        }
        $sum += $ws
        $names += $p.ProcessName
        if ($ws -gt $peakSingle) {
            $peakSingle = $ws
            $peakSingleName = $p.ProcessName
        }
    }
    if ($sum -gt $peakSum) {
        $peakSum = $sum
        $peakSumAt = ($names | Sort-Object -Unique) -join ', '
    }
    Start-Sleep -Milliseconds 500
}
'peak sum of test binaries : {0:N1} MB ({1})' -f ($peakSum / 1MB), $peakSumAt
'peak single test binary   : {0:N1} MB ({1})' -f ($peakSingle / 1MB), $peakSingleName
