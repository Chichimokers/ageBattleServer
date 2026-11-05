# Records the peak working set of every Go test binary while the suite runs, so
# the 500 MB ceiling in the plan's definition of done is a measurement and not an
# assumption. It samples every 500 ms and prints the maximum at the end.
#
# PeakWorkingSet64 is the process's own high-water mark, so a 500 ms sample cannot
# miss a spike between two samples: the figure is the peak, not the last reading.
#
# IT IS NOT FREE. Enumerating every process on the machine twice a second is
# measurable load, and it was enough to flip one verdict: a full run with this
# sampler alongside it failed TestSyntheticCaptureMatchesOriginal, whose fixture is
# a one-second capture and whose ack grouping is decided by whether a 125 ms tick
# lands between two pages. Run it on a suite whose verdicts you are not about to
# trust, or run it alone and take the memory figure from that run.
param([int]$Minutes = 30)

$deadline = (Get-Date).AddMinutes($Minutes)
$peak = 0
$peakName = ''
while ((Get-Date) -lt $deadline) {
    foreach ($p in (Get-Process -ErrorAction SilentlyContinue)) {
        if ($p.ProcessName -notlike '*test*') { continue }
        try {
            $ws = $p.PeakWorkingSet64
        } catch {
            continue
        }
        if ($ws -gt $peak) {
            $peak = $ws
            $peakName = $p.ProcessName
        }
    }
    Start-Sleep -Milliseconds 500
}
'peak working set: {0:N1} MB ({1})' -f ($peak / 1MB), $peakName
