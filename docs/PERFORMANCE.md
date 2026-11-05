# Performance: CPU, RAM, allocations, goroutines

> **What this file covers.** The cost figures of the server and of the test suite, **the
> methodology they were measured with**, and the caps that exist because they were measured. It is the
> file you read when the question is "how much does it cost" or "does this fit the budget".
>
> **Status.** Current. The figures here are the most recent measurement **for each
> thing**, and every table says which measurement it comes from: a table from the 37 followed by two
> representation changes (the 39.3 and the 40.2) is a "before", and its use is the before/after,
> not the number. Every figure says in which section of `SESSION-JOURNAL.md` it was measured and with what
> machine load, because a figure without the load it was taken under is comparable to
> nothing.
>
> **Evidence.** `SESSION-JOURNAL.md §34` (long match), `§35` (goroutines), `§36.5`/`§36.6`,
> `§37` (clients), `§38` (the tick), `§39` (the profile), `§40.2` (the peers table),
> `§40.5` (the final measurement). The test files are the primary source and carry the
> number next to them.
>
> **A rule that applies to everything here** (rule 7 of `AGENTS.md`): a test that brings the
> machine down is a broken test even if it passes. Nothing generates synthetic data without an explicit cap in
> the code, and no O(n²)-memory algorithms over real captures.
>
> **The suite is currently over the budget** (778,5 s against 600 s) and **section 9** is
> the write-up of the ways out, with their measured figures. **None of them has been taken**:
> which tests execute is the user's decision, not this file's.

---

## 0. The budget, and why it exists

| | ceiling | why |
|---|---|---|
| full suite | **10 min and ~500 MB** | the ceiling is the **suite's**, not a test's |
| short loop (`go test -short ./...`) | ~45 s, its own budget | it is separate, not a share |
| one test | a few hundred MB | past that it is treated as a **failure**, and said before launching it |

The reason is written down because it was paid for: an unverified harness test reached **20 GB of
RAM** and had to be stopped by hand. What was measured, not assumed:

| state | peak RAM |
|---|---|
| `internal/sim` committed, over the 1m29s capture | **49 MB** |
| `internal/sim` committed, over the synthetic capture | **33 MB** |
| unverified work (discarded) | **20,000 MB** |

That is, **the harness we use does not have the problem**. What caused it was the trimming
work, which was left unverified and thrown away whole (`git clean`). The exact line that
grows could not be located: `matchEvents` generates about 2,600 small events, which cannot be
20 GB, so the growth is downstream, in the `Render()` or in `compare.go`'s diff. **The diff
resynchronises at every step**, which is exactly what makes it O(n²), so it is a
real risk of the committed code and not only of the scratch.

## 1. The final measurement, with the machine load in place

`SESSION-JOURNAL.md §40.5`. **Warning that goes first**: that session found the machine at **100% CPU**
with an unrelated process during the first half, and at **15-38%** at the end. The clock
figures of the two stretches **are not comparable with each other**; the RAM ones are, because
they are sums of working sets.

| | 39.8 (loaded machine) | 40.5 | **44 (2026-10-05)** | **45 (2026-10-05, this tree)** | note |
|---|---|---|---|---|---|
| `go test -short ./... -count=1` | 97.3 s / 117.4 MB | **43.3 / 44.1 / 46.7 / 58.6 s** over four runs / **113.2-117.4 MB** | **52.3, 54.6 and 55.1 s** over three runs / **112.3 MB** | **~49 s / 113.6 MB** | the 44's three clock figures are **NOT comparable** to the 40.5 row; see the load below |
| `go test ./... -count=1 -timeout 25m` | 791.5 s / 116.7 MB | **770.4 s / 115.7 MB** | **778.5 s / 116.6 MB** | **775.6 s wall clock / 116.0 MB** | `internal/sim` **770.809 s**, which is the critical path |
| `go test -race ./internal/server/ ./internal/session/ -count=1 -timeout 25m` | 157 s (server+session) | **38.7 s + 52.5 s, 0 races** | **39.5 + 54.0 + 779.7 s, 0 races** (with `sim`) | **38.602 s + 49.194 s, 0 races**, 60.6 s wall | the 45 did not re-run `sim` under `-race`; the 44's 779.7 s stands for that |
| `go run ./cmd/gensim -check internal/sim/testdata` | ok | **ok (exit 0)** | **ok (exit 0)** | **ok (exit 0)** | |
| `go test ./internal/sim/ -count=1 -v` | — | — | 771.0 s | **772.6 s, MUST-MATCH 0 on all seven** | the evidence for the byte-for-byte claim |

### The load of the 45, and what its clock may be compared with

Eight samples of `Win32_Processor.LoadPercentage` while the 45's full suite ran, taken by the
sampler itself: **not sampled this round**, so the machine's load during the 775,6 s is
**not on record**. What is on record is that the full suite's clock is **comparable** to the
44's and to the 40.5's, for the reason the 44 gave and which the 45's numbers confirm: the
suite is dominated by **real-time waits**, not by CPU. **775,6 s against 778,5 s is
−2,9 s**, and `internal/sim` at **770,809 s against 772,3 s** is −1,5 s. Two independent runs
of the same tree are within 0,4 % of each other, which is what "load-independent" means here.
**The short loop's clock is the one that is not comparable**, for the same reason as always:
it is CPU-bound.

### The load of the 44, measured, and why one of the two rows is not comparable

Eight samples of `Win32_Processor.LoadPercentage` taken while this shell did nothing but ask
for them: **69, 54, 71, 60, 71, 68, 57, 68 %**. The machine was **not** quiet, and the counter
that says so is not this suite's doing. So:

- **The short loop's clock is not comparable** to the 40.5 row. It is CPU-bound, it went
  from 43.3 s to **52.3 / 54.6 / 55.1 s** over three runs, and the idle load counter still
  read **71%** on the last of them. **Nothing is concluded from that difference** — not a
  regression, not an improvement. Its critical path, `internal/sim`, ranged 43.3-46.9 s on the
  same tree in the same session, which is the same story from the other side.
- **The full suite's clock IS comparable**, because it is dominated by **real-time waits**,
  not by CPU: the five replays sleep. 778.5 s against 770.4 s is +8.1 s, and the harness
  change in this round explains most of it exactly: **the settle grew by 850 ms** (it is now
  the observer step's period, `interval+2` ticks, `internal/sim/replay.go`), over **seven**
  replays, which is +5.95 s of pure added sleep. The rest is the load.
- **The RAM figures are comparable to everything**, because they are sums of working sets and
  not of elapsed time. 112.3 MB short loop and 116.6 MB full suite, against 115.7 MB and
  113.2-117.4 MB before.

The sampler is the one of section 2: each test binary by **its own executable path** under
`%TEMP%\go-build*\b*\*.test.exe`, read with `Process.MainModule.FileName`, sampling every
400 ms **from the shell** while `go test` runs in the foreground. 1328 samples over the full
run, 1316 non-zero, **max 4 concurrent binaries**, peak summed working set 116.6 MB. The
non-zero count is the check that the filter matched: a filter that matches nothing reports 0
MB with the same confidence.

**The 45's sampler, and it is the filter that `AGENTS.md` asks for**: each test binary is
counted only if the **command line of its parent** carries the repository path, which is why
`go` is launched as **`go -C <repo>`** rather than from the working directory — launched from
the working directory the parent is a bare `go test ./...` with no path, the filter matches
nothing, and it reports 0 MB with full confidence, which is the failure this sampler exists
to prevent. Run on the full suite: **1062 samples, 1053 with at least one match, max 4
concurrent binaries**, `proxy.test`, `server.test`, `session.test`, `sim.test`,
`battleserver.test` and `gensim.test` all seen by name, **peak 116,0 MB**. The short loop
the same way: **62 samples, 51 non-zero, 113,6 MB**.

Check from the 44 (2026-10-05, on this tree): all of `internal/sim`, the seven captures plus
the fixtures, in **771.0 s** (`-v`) with Must-MATCH **0** in the five real ones and in the two
synthetic ones. The per-capture clocks are in [`CAPTURES.md`](CAPTURES.md), which is where
that table lives.

The **critical path** is `internal/sim` (772 s of the 778), because it replays the captures in
real time on purpose: the server emits `0x0A` per tick, not per input
(`Options.ReplaySpeed`), and speeding the replay up would measure the harness and not the server.
`session` and `server` run **in parallel** with it, so they do not move the suite's clock.

**Warning about section 3's tables**: the per-tick cost ones and the per-stage distribution
ones are the most recent measurement (`TestTickCostAtEveryPeerCount` and
`TestTickCostSplitsIntoStages`, run on 2026-10-04 on this tree), and those of section 4 are
**from before the two representation changes**, which is what makes them useful: they are the "before"
of a before/after. Every table says which measurement it comes from.

## 2. The RAM sampler, and the three ways that report 0 MB without warning

A filter that matches nothing reports a zero as confidently as the thing it measures, and that
was measured three times. The filter that **does** work is: each test binary by **its own
executable path** (a temporary `go-build` one, read with `Process.MainModule.FileName`, which is
`OpenProcess` + `QueryFullImageName` and **not** WMI), and the sampling running **in the shell**
while `go test` goes in a `Start-Job`.

| form | result | why |
|---|---|---|
| `Start-Job` with the filter inside | **0 MB**, 103 samples, 0 matches | a job sees the processes, but the array of names does not cross the job's serialisation: it arrives as `System.Collections.ArrayList`, and the comparison is against that string |
| `Start-Process pwsh` with the sampler inside | **0 MB** | the child sees **one** process, its own |
| the package names as they are (`sim`) | **0 MB**, 143 matches, 0 summed | Windows strips `.exe` but **keeps** `.test`: the name is `sim.test` |

And a fourth warning, the one about filtering by `*.test.exe`: on Windows `Get-Process` returns the
name **without** extension, so a filter on `*.test.exe` gives 0 MB. A filter on the
bare name, on the other hand, picks up other projects' `*.test`.

## 3. What a tick costs

`src_go/internal/session/tickcost_test.go`. Fixture: the sweep one from the 37
(`sweepFixture`, same peers, same 150 B of upload per peer and tick, same four
checksum records), `-simulationPeriod 125`, which is what every capture records.

**Clock discipline is not optional**: this machine's monotonic clock is
quantised, and timing a tick gives 0 most of the time (measured there: 19 zeros out of 55 buckets
of a single tick). Every figure is a whole window of **4096 ticks** with **one** pair of
readings, repeated 3 times and keeping the fastest window: machine load only adds
time. The two counters (allocations and bytes) come from `MemStats` and are
exact.

| peers | blocks | ns/tick | allocs/tick | allocs/peer | B/tick |
|---|---|---|---|---|---|
| 1 | 8 | 1,066 | 4.00 | 4.00 | 422 |
| 2 | 8 | 1,497 | 6.00 | 3.00 | 777 |
| 4 | 8 | 2,470 | 6.00 | 1.50 | 1,497 |
| 8 | 8 | 4,026 | 6.01 | 0.75 | 2,666 |
| 16 | 16 | 8,466 | 6.02 | 0.38 | 5,534 |
| 32 | 32 | 15,931 | 6.05 | 0.19 | 11,015 |

The blocks come from the binary's formula in section 5, not from a choice of the
test.

- **pending time** over the points at or above the 8-peer floor:
  **492 ns/peer** (0.49 us), against the 30,000 ns cap the test itself sets.
- **pending allocations**: **0.00 objects/peer/tick**, against the 3.0 cap (0.50 with
  `-race`, which is its own shadow). The fixed part of a tick is **4 objects** and **does not grow**
  with the peers.
- With `-race` the same tick at 32 peers measures 585 us, that is **about thirteen times more**. That is why
  the clock caps of that file (2 ms per tick, 30 us per peer) are **order-of-magnitude** caps
  and say so: the sharp assertion of the file is **the count**, which is a
  counter and not a clock.

### The extrapolation, with the arithmetic shown

Measured at 4 peers: 2,470 ns/tick. Ticks of a 2 h match at 8 ticks/s =
`2 x 3600 x 8 = 57,600`. CPU = `57,600 x 2,470 ns = 0.142 s` **for the whole match**.
Measured at 8 peers: 4,026 ns/tick -> `0.232 s`.

That is, **a two-hour match costs between 0.14 and 0.23 seconds of CPU**, with a tick
every 125 ms. The calculation is a tautology (it is the per-tick times the number of ticks) and
so what matters is **where the per-tick comes from**.

**What is measured and what is extrapolated.** Measured: the whole table, the slopes, the live heap
per point, the bytes and allocations per tick, and the number of blocks. Extrapolated: the
CPU seconds per match. What would make the extrapolation false, **in order of how much**:

1. **The size of the ack body.** The fixture uploads 150 B per peer and tick and produces an ack
   of 193 B with one peer and of 4,936 B with 32. A real ack in a 4-player
   match can be a few KiB. The store is capped in **bytes**, so a bigger body **does not
   cost more heap**: it costs more CPU per tick (copy of the body, memcpy into
   the store, more bytes on the wire). The CPU figure above is a **floor**, and the floor grows
   with the body.
2. **That all the players upload on EVERY tick.** The other way round here: a real match
   uploads in bursts, so the table is the **ceiling** and not the average.
3. **That the main loop is the only per-tick cost.** It is for the session
   work; connections have their own cost (16 KiB of heap, and 34-36 KiB of heap plus stacks per
   WebSocket connection, with 2 goroutines).
4. **One session per core.** The main loop is **one** goroutine
   (`internal/server/server.go:366`), so N concurrent matches share the same
   tick: the total CPU is the sum and the wall clock is not.

### The per-stage distribution, with one stage removed each time

Round-robin windows (one per mode and repetition) so that a drift of the machine does not fall
on one mode and not on the others. A mode with no work has to come out cheaper and one with
work more expensive: if a sign comes out the other way, the table is noise and the test says so.

**Warning that goes first, and it is from the test itself**: in this run the **`NOT RESOLVED`** came
out in both points. This machine's clock has a spread between windows of the same
mode of **7,862 ns at 8 peers** and **4,445 ns at 32**, and the smallest stage is 1,254 and
2,825 ns: **this table does not separate the stages of the clock**, and the sign assertions pass
because the tolerance covers the noise, not because the stages are measured. They are given
because the order and the sign are reproducible, and because the number that comes out of each mode is the same
one the 39.7 printed.

**8 peers** (windows of 2048 ticks, the fastest of 3):

| stage | ns/tick | allocs/tick | delta ns | per peer |
|---|---|---|---|---|
| full tick (1 upload + 4 checksums) | 3,447 | 6.01 | - | - |
| 1 upload and 1 checksum record per peer | 2,192 | 6.01 | -1,254 | -157 ns per extra checksum round |
| no uploads (heartbeat ack) | 1,592 | 4.00 | -1,855 | -232 ns per flush, 0.25 allocs |
| 4 uploads per peer | 8,026 | 6.07 | +4,580 | **+268 ns** per extra sub |
| 256 observers served | 5,015 | 6.01 | +1,569 | 54 ns per visit (29 visits per tick) |

**32 peers** (the same five modes):

| stage | ns/tick | allocs/tick | delta ns | per peer |
|---|---|---|---|---|
| full tick | 14,939 | 6.01 | - | - |
| 1 upload and 1 checksum record per peer | 12,114 | 6.01 | -2,825 | -88 ns |
| no uploads | 6,177 | 4.00 | -8,762 | -274 ns, 0.06 allocs |
| 4 uploads per peer | 33,806 | 6.15 | +18,867 | **+288 ns** per extra sub |
| 256 observers | 16,642 | 6.01 | +1,703 | 59 ns per visit |

**Which part dominates: the draining of each peer's queue and the building of the body.** One
extra sub per peer costs 268-288 ns and is the most expensive thing measurable here: with eight
players it is 2.1 us of the tick's 3.4, and with thirty-two it is 9.2 of the 14.9. Reconciliation
is 88-157 ns per peer for three rounds, that is **about 30-50 ns per round and
peer**, and it is not comparable with the draining.

**Reconciliation is linear in the peers, and it is proven by both magnitudes, not
by reading the code.** `AckState.ReconcileChecksums` compares the front record of each
active peer against that of the **first** peer and nothing else: a `bytes.Equal` of 8 bytes per peer and
round. If it were a pairwise check, the per-peer term would grow when the peers are
multiplied by four, and here it is 157 ns at 8 peers and 88 ns at 32: **it does not grow, and
it even drops**. Its cost in allocations is **0.00 per peer**.

**Serving the observer is the cheapest stage and does not scale with the number of
observers**: 54-59 ns per visit for 29 visits per tick, and **0.00 allocations per
tick** with 256 observers served.

## 4. Where the CPU goes, with FUNCTION names

**What you have to know before reading the table**: it is the 39.1's measurement, and it is the
**"before"** of the two representation changes that were made with it (the three `AckState`
tables, §39.3, and `Session.Peers`, §40.2). That is why `mapaccess2` comes out at 11%: **today there is
no `mapaccess` on the tick's path**, and the two halves attributed to it no longer
exist. It is kept as the "before" of section 4.3, which is where the "after" of both is.

`internal/session/tickprofile_test.go` (the tick, with the 37/38 fixture) and
`internal/server/serverprofile_test.go` (a real server, with real TCP clients). They are
**two halves that do not overlap**. Each profile is **1.2 s of WALL CLOCK** in the first and 3 s in
the second, and that is what makes the parts comparable between modes: equal time is
equal samples. The sampling rate is raised to 1000 Hz before `StartCPUProfile` (at
100 Hz that window would carry 120 samples).

With what was gained, over the 38.2 fixture, 32 peers, full tick:

| function | flat | cumulative |
|---|---|---|
| `runtime.mapaccess2` | **11.02%** | **36.41%** |
| `internal/runtime/maps.memHashAES` | **10.18%** | 12.17% |
| `maps.ctrlGroup.matchH2` | 4.72% | |
| `maps.(*groupReference).key` | 3.46% | |
| `runtime.memequal8` | 3.25% | |
| `(*AckState).QueueEmpty` | 2.52% | 7.35% |
| `runtime.memmove` | 2.52% | |
| `(*AckState).QueueEntries` | 2.20% | 6.09% |
| `(*AckState).ReconcileChecksums` | 2.20% | **24.13%** |
| `(*AckState).PopTick` | 0.94% | 9.76% |
| `(*AckState).QueueDataSub` | 3.20% | 8.83% |

Per-stage distribution with the clock in place (cumulative over the whole window):

| stage | % of the window | where it is inside |
|---|---|---|
| `Session.PopAckTick` | **48.5%** | `queueEntries` 25.8% + `ReconcileChecksums` 24.1% |
| `Session.Tick` | 18.1% | `closeSyncTimedOutPeers` 15.5%, almost all `syncTimedOut` |
| `Session.QueueGameData` | 9.4% | `QueueDataSub` |
| `AckState.QueueEmpty` | 7.4% | |

**Where the map is**: `ReconcileChecksums` spends 172 ms of its 230 in a `mapaccess1` (that is
75%), and it is two lookups per round. `QueueEmpty` 33 of 70, `QueueDataSub` 19 of 90,
`releaseSub` 19, `QueueEntries` 18, `LastSync` 21 of one `mapaccess2`. Summed: **282 ms of
953 = 29.6% of the tick**. The other 82 ms (8.6%) are `Session.Peers`.

**And one thing that is NOT what it looks like**: the `lock2` + `semasleep` +
`lockWithRank` block (15% of the window, which looks like a very expensive mutex) is **the runtime's own
lock**. `semasleep` comes 100% from `lock2`, and `lock2` 100% from `lockWithRank`, and their
callers are `gcMarkTermination` and `findRunnable`. The session's lock is
`sync.(*Mutex).Lock` at **0.16%**. That is: **the session mutex's cost is NOT the
problem**, and half a window of the "synchronisation cost" you see in a profile of this
code is the collector and the scheduler.

### The two changes that were made with this: -42% and -42%

**The first, the three `AckState` tables** (`SESSION-JOURNAL.md §39.3`).
`queued map[byte]*slotQueue`, `perPeer map[byte]*checksumQueue` and
`lastSync map[byte]time.Time` are now `[256]...`. The key is a byte and the domain is
256: **a map is a hash of a value that is already a perfect index**. It is the same argument
as `rank [256]int16`.

| peers | ns/tick before | ns/tick after | delta | allocs/tick | B/tick | live heap |
|---|---|---|---|---|---|---|
| 1 | 1,699 | 1,238 | **-27%** | 4.00 | 422 | +15 KiB |
| 2 | 2,702 | 2,244 | -17% | 6.00 | 777 | +16 KiB |
| 4 | 4,164 | 3,225 | **-23%** | 6.00 | 1,497 | +16 KiB |
| 8 | 8,926 | 5,927 | **-34%** | 6.01 | 2,667 | +15 KiB |
| 16 | 19,884 | 11,266 | -43% | 6.02 | 5,535 | +14 KiB |
| 32 | 36,070 | 20,932 | **-42%** | 6.06 | 11,017 | +12 KiB |

**slope per peer: 1,114 ns -> 622 ns (-44%)**. Allocations and bytes **do not** move.
2 h match: 0.240 s -> **0.186 s** of CPU at four players, and 0.514 s -> **0.341 s** at
eight.

What is paid is **+15 KiB of live heap per session, and it is FLAT in the peers**, because an
`AckState` is one per match and not one per client: the per-peer slope stays at **0.00
objects per peer and tick**.

**The falsification**, on both sides that exist: with the maps back in (the defect
reintroduced by hand) 36,070 ns/tick at 32 peers, a slope of 1,114 ns/peer, and the profile brings
`mapaccess2` back to 36.4% and `memHashAES` to 10.2%. With the change in,
**the five committed captures of `internal/sim`**, which is the byte-for-byte
comparison against the original: an array where there was a map changes what is read if the map did not return
the same thing, and that comes out as a must-match. It did not come out.

**The second, `Session.Peers`** (`SESSION-JOURNAL.md §40.2`), which is what makes the `mapaccess2` of the
table above history today. `Peers` goes from `map[byte]*Peer` to
`peerSlots [256]*Peer`, indexed by player slot, and **every** iteration goes through
`peerOrder` (the join order, which is that of the original's queue vector,
`session+2192`). The argument is not about speed: it is that **that is the original's shape**
(`sub_1400E5670:100` writes `session + 24 + 8*peerID`, an 8-byte slot per id, and there is no
hash table on that path), and what forces it is that a slot table has no order, so
every iteration has to say which one it wants.

| peers | ns/tick before | ns/tick after | delta |
|---|---|---|---|
| 1 | 1,205 | 1,000 | **-17%** |
| 2 | 2,534 | 1,550 | **-39%** |
| 4 | 4,383 | 2,385 | **-46%** |
| 8 | 7,302 | 4,043 | **-45%** |
| 16 | 12,871 | 7,996 | **-38%** |
| 32 | 25,271 | 14,636 | **-42%** |

Slope per peer: 748 ns -> 441 ns. Allocations and bytes per tick **do not move**
(4.00 / 6.05 objects and 422 / 11,016 B). And here the justification that was missing in the 39.7 is
**proven on both sides**: the path that reaches the wire (`queueEntries` ->
`peerOrder`) never read the map, before or now, and the five places that did walk it all passed
the **same body to every peer**, with the ack and the notice in **two sequential
passes**, and the harness's comparison is **per connection**. The only two that did read it
(`PeerBySecret` and `PeerByKey`) have in the original a deterministic order that the map did not have.

### The tick does not show up in the server's profile, and the arithmetic is the reason

`-simulationPeriod 125` is 8 ticks per second, and at 4 peers a tick measures ~3.2 us, so
a 3 s window carries `3 x 8 x 3.2 us = ~77 us` of tick in 3 s of clock, **0.0026%**.
At 1000 Hz that is 0.08 expected samples. Measured: the server's window carried **16 to 54
samples in total** (the whole process, **0.53% of a core**), with the tick appearing once.
**One sample is not a measurement**: the tick's attribution is that of the `session`
file and the server's is everything else's, and they are reported side by side, not mixed.

## 5. Memory

### Where the short-lived is

With `runtime.MemProfileRate = 1` (**not** the default 512 KiB: at 512 KiB the window's 48.75 MB of
come out ranked in TWO places and the 4 to 6 objects per tick **do not appear**, because
6 KiB against a 512 KiB interval is a 1% probability — the profile was saying "nothing is
left" for the wrong reason: it could not see what was left).

Short (4096 ticks, everything allocated), 32 peers:

| site | objects | per tick | bytes | % of the bytes |
|---|---|---|---|---|
| `(*AckState).PopTick` | 12,306 | **3.00** | 21,600 KiB | 47.8% |
| `(*Session).queueEntries` | 8,192 | **2.00** | 768 KiB | 1.7% |
| `(*Replay).AddFrame` | 4,106 | **1.00** | 21,509 KiB | 47.6% |
| `internal/reflectlite.Swapper` (the `sort.SliceStable`) | 4,096 | 1.00 | 128 KiB | 0.3% |

**6 objects per tick and per SESSION, none per peer.** The three of `PopTick` are the ack
body, the `sort` closure and the `sort` swapper. And the bytes: **95.4% are
the ack body and the copy the store makes**, which is exactly what **cannot** be pooled
by property (it leaves the file, N peers receive it, and nobody outside says "I do not
need it any more"). See section 7 of this file.

This table is from the 39.2, with `Session.Peers` still a map. What the 40.2's
measurement adds is that **the allocations and the bytes per tick did not move** with the two
representation changes (4.00 objects at one peer and 6.05 at 32, 422 B and 11,016 B), so the
table still holds as a number.

### Where the long-lived is

`inuse_space` after two collections, the store full and the fixture alive:

| | 8 peers | 32 peers |
|---|---|---|
| `(*Replay).AddFrame` | **480 KiB (99.1%)** | **2.12 MB (99.7%)** |
| everything else | 4 KiB | 2 KiB |
| live objects, in total | 391 | **421** |

That is: **everything that lives is the store, and it is 421 objects in the whole session**. There is
no leak, no accumulation per tick, and the only long-lived thing is the structure that is
already capped in bytes. **The conclusion about memory is that there is nothing left to
attack**, and now it is **measured** and not assumed. The 45 MiB of a 32-peer window are **CHURN**, not heap.

### The curve of a long match

`internal/session/longmatch_test.go`: two hours of match (57,600 ticks) in one second, without
sleeping. **The server does not leak, does not drift and does not get more expensive over two hours.**

The seven caps that hold that test down, with the margin that chose them:

| cap | value | measured | margin | what it catches |
|---|---|---|---|---|
| heap growth | 4 MiB | 21 KiB | 190x | any leak that reserves something per tick (4 MiB / 57,600 = 72 B) |
| curve slope | 8 B/tick | 0.30 B/tick | 26x | a one-slice-header-per-tick leak (48 B) already fails it |
| absolute heap ceiling | 32 MiB | 820 KiB | 40x | a leak that is already in the baseline |
| allocations per tick | 24 (after the 38) | 6.01 | 4x | a new path that reserves per tick |
| allocation growth | 8 per tick | **-0.03** | — | constant degradation |
| cost per tick | 3x + 5,000 ns | <= 1.02 with no load / 1.56 with the whole short loop in parallel | — | an order-of-magnitude degradation |
| cost per visit | 3x + 5,000 ns | <= 1.01 | — | the same on the observer's path |

Each value is a constant of the file, next to the assertion: `longMatchHeapGrowthBudget`,
`longMatchHeapSlopeBudget`, `longMatchHeapCeiling`, `longMatchMaxAllocsPerTick`,
`longMatchAllocGrowthBudget`, `longMatchTimeGrowthRatio` + `longMatchTimeFloorBudget` and
`longMatchVisitGrowthRatio` + `longMatchVisitFloorBudget`. There is an eighth, in the
residue test: `longMatchChurnResidueBudget = 64 KiB` for a thousand observers that come and
go (`longMatchChurnObservers = 1000`).

**The caps are tightened because the figure that justified them no longer exists.** The slope of
allocations per peer goes from 12.0 to **3.0** (measured 0.00 without the detector and 0.50 with it), and the
absolute per tick from 400 to **24** (measured 6.05). With the old caps the first of the
falsifications **passes**, which is exactly what a cap with twelve times of slack does.

**What that file does NOT measure**, written down so that the next session does not read it as more
than it is:

1. **The sync timeout.** The clock inside the session is the wall one
   (`queueEntries` takes a `time.Time` and `syncTimedOut` subtracts it), so two hours in
   two seconds cannot make a peer expire at 60 s.
2. **Compile-time wraps and integer widths.** A 2 h horizon is 57,600
   frames; `session+2216`, `session+2296` and the ring index are a long way from
   any limit. A test that took those counters to the limit would have to synthesise
   billions of frames, which is exactly the mistake of §25.
3. **The clock of the measurement itself** is wall and is quantised (see section 3).

### The store's cap: the formula, from the binary

```
sub_1400E2590:221   v17 = (a1[1] - *a1) >> 3      the length of the peer slot vector
sub_1400E2590:235   if (v17 < 8) v17 = 8            the floor
sub_1400E2590:241   v21 = ceilf(v20 * *(float *)(a1 + 2168))   f, as a float
sub_1400E2590:255   sub_1400D4E10(store, v21, a1[269], a1[270])   maxBlocks, session+2152, session+2160
sub_1400D4E10:18    a1[1] = a3                        blockSize = session+2152
sub_1400D4E10:19    a1[2] = a2                        maxBlocks = the ceilf above
main_1400d1310:346  v43 = 1.0                      the default of f
main_1400d1310:347  v93 = 0x10000                  the default of -pageSize
main_1400d1310:617  v43 = (float)atoi(v49) * 0.125 -maxPagesToStorePerSession / 8
main_1400d1310:818  f's bits go down to the store
```

| peers | blocks | window |
|---|---|---|
| 1, 2, 4, 8 | **8** (the floor) | 512 KiB |
| 16 | 16 | 1 MiB |
| 32 | 32 | 2 MiB |

**64 KiB per peer above the floor of 8, and nothing below it**, and `f` comes from the
configuration: the ceiling is the operator's, not the code's.

### The measured sweep table (1 to 32 peers)

`internal/session/peersweep_test.go`. The store is measured at the **peak** of its sawtooth (the store
grows until the last block cannot hold one more frame and then loses a whole block),
with the peak taken and the **minimum of two passes**, and with the live heap **after two forced GCs with the fixture
alive** (`runtime.KeepAlive` behind the read: without it, the heap being measured is the test
binary's and not the session's).

**Which measurement it comes from**: `SESSION-JOURNAL.md §37.2/§37.3`, that is **before** the tick stopped
allocating per client (§38.3). The `allocs/t` column is what says so: they are 17 to 310 objects per
tick, and today's code measures **6.01** at 8 peers. The store and heap columns **do**
still hold, and the slopes below are the ones that bound the store.

```
peers blocks  window  store KiB charged KiB  frames  heap KiB  allocs/t  fill  ticks
    1      8    512K        497.8        511.7    2096      805       17.00  1835  2603
    2      8    512K        493.6        510.0    1296      795       30.00  1135  1903
    4      8    512K        538.1        509.6     736      819       49.00   645  1413
    8      8    512K        508.1        505.7     392      812       86.00   344  1112
   16     16   1024K       1068.2        994.1     400     1382      165.02   376  1144
   32     32   2048K       2202.5       2028.4     416     2484      310.09   404  1172
```

**The slope, which is what is asserted** (least squares over the 8, 16 and 32-peer points):

| slope | measured | formula | ratio |
|---|---|---|---|
| store, **charged** bytes | 65,144 B/peer | 65,536 | **0.99x** |
| store, **actual** bytes (Go size classes) | 72,337 B/peer | 65,536 | **1.10x** |
| the process's live heap | 71,238 B/peer | 65,536 | **1.09x** |
| allocations per tick | 9.30 objects/peer (9.81 with `-race`) | - | - |

The difference between the first and the second row **is not spare memory**: it is the
rounding to size classes. An ack of 193 bytes falls in the 208 class (7.8%) and one of 4,936
in the 5,376 one (8.9%), and since the frame grows with the number of peers, the actual-bytes
slope comes out 10% above the charged-bytes one. The charged-bytes one is the unit of the
formula (`vmethod_20:76` subtracts `a3 + 57` per frame) and that is why it has the narrower band.

**The 9.30 objects per peer and tick of that table are history**: they were brought down to **0.00**. See
section 4 and `SESSION-JOURNAL.md §38.3`.

### The cost of a LIVE connection

The sweep does not cover it, because the sweep has no sockets: 16 KiB of heap, and 34-36 KiB of
heap plus stacks per WebSocket connection, with **2 goroutines** (a `readLoop` and a
`writeLoop`). The observer **is not a peer**: it is not in the session's array and it does not consume
from the upload queue.

### The counters that reach nothing

Five fields of the original's session and connection objects that **no byte on the wire depends
on**, so that they are not divergences and are not in
[`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) any more. They are here because
the field-by-field audit (`SESSION-JOURNAL.md §32.5`, `§32.8`) produced them and the knowledge should not
be lost with the classification, and because "we do not read it" is a decision somebody has to
be able to check.

| field | what it is | who writes it | who reads it | cost |
|---|---|---|---|---|
| `session+2140` | the "already published" latch of the replay uploader: one byte, one getter and one setter (`sub_1400E7600`, `sub_1400E7610`) with **a single pair of calls**, both in `sub_14010F4F0` (`:99` on entry, `:273` on exit) | `sub_14010F4F0` | itself only | 1 B/session |
| `session+2176` | ticks this server has run through | the munin itself (`E9E90:153`) | `E9E90` | 4 B |
| `session+2224` | ack bytes sent | the tick, when serialising the `0x0A` (`E2590:361`) | `E9E90` | 4 B |
| `session+2232` | LobbyConnects accepted | `EB5F0:136` | `E9E90` | 4 B |
| `session+2240` | the **widest span anyone has asked for**, kept as a maximum (`E3260:32-36`) | the observer frame-resend path | `E9E90` | 4 B |

The whole set is **17 bytes per session object** and it is not implemented here, which is why it
is not a divergence either way: with no reader on the wire, a client, a peer and the replay host
cannot tell the two implementations apart. `+2240` is the one whose *name* is still unknown,
along with `+2140`'s metric; both questions stay on the work list ([`PENDING.md`](PENDING.md)
2.1 and 2.3) and both are binary reading, with no capture needed.

**What decided it, and it is the same test every entry gets**: a divergence is a difference
**visible from outside**, and the difference between the counters above and `conn+88` was
that the latter is read by the dispatcher's own gate. **That is no longer enough to keep it**,
because `conn+88` was settled on 2026-10-05 and the gate turns out to be unreachable for a
connection that speaks the protocol. It is the next subsection.

### `conn+88`, settled

One byte of the connection object, and it is the only field of the audit that decided what a
connection is **answered** to. It means **"this connection has completed its `LobbyConnect`"**,
and it was open for one day because the premise it was filed under was a misreading.

| | |
|---|---|
| who writes 1 | `sub_1400EB5F0:469` and `:490`, and **nothing else**: every byte-sized store to `+88` in the 7,450 files of `src/` is those two plus `sub_140233400:61`, which writes **0** into a `std::string` |
| under what condition | both require the `LobbyConnect`'s dword at `msg+80` to be **0**, i.e. a new connection and not a reconnect. `:469` is `session known && no peer for that secret`; `:490` is `v13 == 0`, **no session at all**, which is the first connect. `v13` **is** the session, positively: the lookup at `EB5F0:87-111` keys a tree on `(msg+48, msg+56)` = the message's `titleID` and `sessionID` (`MESSAGES.md:149-150`) and yields `v11[6]`, and `:506`'s string, reached exactly when it is 0, is `"Attempt to reconnect to a non-existent sessionID"` |
| who reads it | `sub_1400DE050:136`, and only there: `if (conn+88 == 0) { if (type != 21) return 0; ... }`. `21` is `LobbyConnect` (`0x15`, `MESSAGES.md:60`). After the handler it **falls through** to the normal dispatch at `:248`, there is no `else` |
| what it costs | 1 byte per connection, and **nothing here**: it is not modelled |
| why that is not a divergence | the drop path needs a connection whose **first** message is not a `LobbyConnect`. Every connection of every committed capture sends `0x15` first, **the two observers included**, so no byte of any capture is compared differently |

The full reading, with the brace depths that settle which `if` the `else if` binds to, is in
[`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 10.2 and
[`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §45.2. One consequence is still open and is
**not** about this byte: the **reconnect** branch (`EB5F0:133-463`) never writes it, so a
reconnect can only work on a connection that already carries it
([`PENDING.md`](PENDING.md) 2.2).

## 6. Goroutines and contention

`internal/server/goroutineleak_test.go` and `internal/server/serverprofile_test.go`.
Real server, 4 real TCP clients, 3 s, `-simulationPeriod 125`, with
`SetMutexProfileFraction(1)` and `SetBlockProfileRate(1)`.

**Goroutines**: 5 with no client at all, **17 with four**, 17 after the window. The 17:

| n | where |
|---|---|
| **4** | `(*Client).readLoop` (`client.go:106`) reading: one per connection |
| **4** | `(*Client).writeLoop` (`client.go:129`): one per connection |
| 4 | this file's own client readers (`serverprofile_test.go:331`) |
| 1 | the pprof profile's own writer |
| the rest | main loop, runtime, GC workers |

That is **TWO server goroutines per connection**. `internal/session` **starts no
goroutine**, and that is why its timeout is covered from the server.

**Contention, which is the question**: out of **95 contended mutex acquisitions** in 3 s,
**8 are ours** and the other 87 are runtime locks (GC and scheduler):

| mutex | events | total |
|---|---|---|
| `runtime.gcMarkTermination -> forEachP -> unlock` | 4 | 0.777 ms |
| `runtime.findRunnable -> unlock` | 17 | 0.584 ms |
| `runtime.stopm -> unlock` | 3 | 0.468 ms |
| **`(*AckState).QueueDataSub` (`ack.go`, `a.mu.Lock()`)** | **8** | **0.306 ms** |
| `(*Client).writeLoop` | 1 | 0.004 ms |

And the block profile, with its 153 events, says the same from the other side: the only
block of ours is the **same** `QueueDataSub`, 8 events, 0.234 ms. The two big ones are **not
contention** but **idle waiting**: `(*Client).writeLoop` waiting for work
(`selectgo`), 12,472 ms over 104 events, and `(*Server).mainLoop` waiting for the next tick,
3,122 ms over 25 events.

**The answer, with numbers**: 8 contended acquisitions of `AckState.mu` in 24 ticks
(3 s at 125 ms), 0.31 ms in total, **0.010% of the window**. And `Session.Tick`,
`updateObserversLocked` and the peers map **do not appear even once** in the mutex profile.
At this scale contention is negligible, and now that is a **figure** and not an opinion.
What does appear, and with more weight, is 87 events on the runtime's own locks, which is the
collector doing its job.

### Go 1.27's `goroutineleak` profile, and its two traps

A whole package (`internal/profiletest`) was needed to be able to **assert** that a profile
has samples, which is the only way a profile of an empty window does not produce a
spotlessly false top-10. The two traps:

1. **`Profile.Count()` runs one refresh behind.** A forced `runtime.GC()` **does not**
   fix it: you have to force the collection and then **ask again**.
2. **Blindness by reachability.** A leak whose primitive stays **reachable** from a
   runnable goroutine is **not seen by the runtime either**. That is why the primary assertion is the
   **count with a cap**, and that is why the `drive` runs **inside a closure** (`runServerCycle` +
   `assertCycleBudget`): if someone writes a test that keeps the `*Server` alive until after
   asking, the profile will say nothing and the count is all that is left.
   `leakGoroutineSlack = 2` is **measured**, not chosen by taste, but it is the most
   fragile figure of the file: it is a count of the whole process, not of this server. If it ever
   starts failing intermittently, the place to look is `settleGoroutineCount`, not
   the cap.

**The result was that there was no leak.** The baseline and the cap are in the code,
next to the number.

## 7. What was left untouched, and why

**And what was retired from the divergence register for not being observable.** The points that
were in `INTENTIONAL-DIVERGENCES.md` as "representation divergences" are
internal decisions: the criterion of the two divergence files is the black box, and here
there is no box. Section 7 of the intentional file lists the same set with its
reason and its history; this list says **how much it costs** and **why it was left that way**.

- **`Session.Peers` is no longer a map.** It is `peerSlots [256]*Peer`, indexed by player
  slot, and every iteration goes through `peerOrder`. The before/after measurement is in section
  4.3 and the reason in `SESSION-JOURNAL.md §40.2`. What was **not** touched, and is said here so that nobody
  looks in the old place: nothing. The 17% of the window that justified keeping the map went
  away with it.
- **The ack body is not pooled**, by property and not out of caution: it leaves
  `internal/session`, N peers receive it, and nobody outside the package says "I do not
  need it any more"; a pool you hand to N times and return once is exactly how the same buffer
  ends up in two messages. It is 95.4% of the bytes of the short-lived memory (section 5). For the
  same reason the `sort.SliceStable` and its swapper are left unpooled (2 objects per
  tick, and only in a flush with mixed slots), the snapshot of the queue records
  (**2** objects per tick and session according to section 5's table), the `ForEachPeer` closure
  (1 object per tick) and `subPool` (converting back to `[]byte` is the interface box from `Put`).
  That the observer visit copied the frame list instead of using `GetFrames` was 31.9
  extra allocations per tick, and the ns cap **does not** see it: that is why the path is the one it is.
- **The per-peer checksum queue grows with no ceiling** if a peer uploads more than ten records
  per tick, because reconciliation drains at most ten per tick
  (`sub_1400E2590:374-381`). **That is what the original does too**, so it is not a
  defect of ours.
- **`-maxPagesToStorePerSession` multiplies the store's ceiling** (`f = flag / 8`, with no upper
  bound): with a very large flag the ceiling is the operator's.
- **The per-peer allocation cap has a declared hole** (one or two it does not catch, and the
  reason is in the code). If it ever has to catch them, the path is **decomposing**:
  measuring the same sweep without checksum records and subtracting, instead of an absolute cap
  on a slope.
- **The high byte of the state the tick returns (`E2590:483`) is not returned.** Only the munin
  reads it (`sub_1400E9E90:69-71`), it does not travel, and the high byte **can never be set**
  (`E2590:285-286` writes it only if the counter is zero, and the only `++` is at `:380`,
  after). It is not a divergence: it is a metrics counter. `SESSION-JOURNAL.md §36.3`.
- **The observer list is emptied when it is cleared, and the original does not empty it** — that is not a
  divergence either: the `0x19` = 4 and the close are done the same way, and the entry goes the same way when the
  connection drops (`sub_1400E52D0:227-242`). What is kept is the **close after
  releasing the lock**, which is where the self-deadlock was.
- **The profiles are not committed.** They are megabytes and they are regenerated; what is committed is
  the number and the function's name.

## 8. The rule of the two assertions that had to be fixed

The two times the work got thinner, a correct assertion broke, and both times what was
fixed was **the way of measuring**, not the tolerance:

1. **A profile's sample count cannot depend on what the machine delivers.**
   It was checked that the profile has samples before asserting anything about it.
2. **The per-stage distribution's tolerance cannot be a fraction of a reference that gets
   cheaper.** With the tick 42% down, a tolerance written as "20% of the reference"
   shrank on its own and started failing without the distribution changing.

## 9. The suite is over the budget: the options, measured, and none of them taken

**The state.** The full suite is **778,5 s** against the **10 minutes (600 s)** of
`AGENTS.md`'s rule 7. RAM is **116,6 MB** against ~500 MB, so **the clock is the whole of
the problem and the memory is not close to it**. The critical path is **`internal/sim`**
(772,3 s of the 778,5), because the five capture replays run **in real time on purpose**:
the server emits `0x0A` per tick and not per input (`Options.ReplaySpeed`), so a faster
replay measures the harness and not the server.

**Nothing in this section was changed.** Which tests execute is not a decision this file
gets to take; it is the user's. What follows is the arithmetic that a decision needs.

### Where the 778,5 s actually is

| | seconds | of the suite |
|---|---|---|
| the five committed capture replays | **678** | 87 % |
| the two synthetic fixtures | **4,5** | 0,6 % |
| the rest of the `internal/sim` package, and every other package | **~96** | 12 % |

The five are the log span plus the settle, from `CAPTURES.md`: **89 + 199 + 249 + 89 + 47 s**
of span, plus **1.25 s** of settle each. **The recorded clock is 87% of the suite**, and
that is the number every option below is fighting over.

### Why the saving is not where it looks: the captures have no trim slack

The obvious move is to trim the captures shorter. **Measured, and it is not available.**
`trimsim -inspect` prints each capture's **control messages** — the handshake and the
`0x19`/`0x1E` — and `CheckTrim` exists precisely because a crop may not lose them. Each
capture's span is already within seconds of its last control message:

| capture | span | its last control message | slack |
|---|---|---|---|
| `2026-09-29_012528` | 3m19s | the **second** observer's `0x15`/`0x1C`/`0x1A`/`0x19`/`0x1E` at **t=3m13s** | ~6 s |
| `2026-09-30_010531` | 4m9s | the observer's five control messages at **t=4m2s** | ~7 s |
| `2026-09-30_183120` | 1m29s | the observer's five control messages at **t=1m16s** | ~13 s |
| `2026-09-30_201122` | 47 s | the first `0x0A` at **47 s**, which is what the 48 s trim was justified for | **0** |
| `2026-09-28_205830` | 1m29s | none after `t=0`: **traffic in every 10 s bucket to the end** (`0x09 x43` / `0x0A x73` in the last one) | **0** |

**Total available slack: ~26 s, and taking it means cutting into the settle's own margin.**
Three of the five are long **because** their evidence happens late: the observer connects at
4m2s of a 4m9s recording, so the capture *is* the run-up to that handshake. The best case
for trimming the committed set is therefore **~26 s**, against a **178,5 s** gap, at the cost
of the prefix property that is the only thing that makes a trim auditable.

### The 24-minute capture on disk would ADD, not save

`captures/2026-09-29_101831_a553ef8fd63c` is **24m44s** of clock and **92,6 MB**, and it is
**not imported**, so it is not in the 778,5 s. Measured with `-inspect`: **one connection,
one player, no observer**, whose only control message is its `0x15` at `t=0`, and `0x0A
x960` every two minutes for the whole run — **the same rate, the same message, the same
connection, for 24 minutes**. Its own report says 0 must-match and 2 extra messages in 24
minutes.

| | clock it adds | what it buys |
|---|---|---|
| trimmed to 1m, `-verify` ACCEPTED | **~61 s** | a second long WebSocket run; **overlaps what `012528` already covers** |
| untrimmed | **~1.484 s** | — |

**It cannot reduce the suite under any option.** It is also the only thing here that would
make the gap worse, which is worth saying because "import the capture we already have" reads
like a free win and is not.

### The options

Each with what it does to the clock and what it costs. **The gap to close is 178,5 s.**

| | option | saving | suite | what it costs |
|---|---|---|---|---|
| **1** | **Replay the five captures concurrently** | the 678 s of *waiting* overlaps; the critical path becomes the **longest single capture**, 250 s | **~350-400 s** | nothing compared: **every byte of every capture is still compared**. It needs `t.Parallel()` in the **generated** `sim_*_test.go` (`cmd/gensim`), so it is a generator change, not a flag. **Unverified**: port independence is designed for (`AGENTS.md`: the harness takes free ports) but five concurrent replays raise CPU contention exactly where it hurts — the settle window and the tick cadence, which must-match depends on |
| **2** | **Skip the settle for the captures with no observer** (`205830`, `201122`) | 2 × (1.25 − 0.40 floor) = **~1.7 s** | ~777 s | a rule, not a trim: the settle exists for the observer step and there is no observer in those two. **Tiny.** Worth doing only if option 1 is refused |
| **3** | **Trim the five committed captures into their slack** | **≤ ~26 s** | ~752 s | **gives up the prefix property.** A trim `CheckTrim` does not ACCEPT must not be imported, and the ones left are inside the settle's margin |
| **4** | **Drop `2026-09-30_010531`** | **−250 s** | **~528 s** | one of the two deliberately-alike observer handshakes. `012528` still covers an observer, so this is **redundancy**, not coverage — but it *is* a coverage decision |
| **5** | **Drop `010531` and `2026-09-30_183120`** | **−340 s** | **~438 s** | as above, and the observer-handshake pair with it |
| **6** | Raise `ReplaySpeed` | large | — | **forbidden**, and it is the load-bearing design decision: the replay is real time *because* a faster one measures the harness |
| **7** | Import the 24m44s capture | **negative** | +61 s to +1.484 s | makes it worse |

### The one that is recommended, and why

**Option 1**, and it is recommended for a reason the others cannot match: **it is the only
one that removes no comparison**. Options 4 and 5 buy the clock by dropping a capture, which
means the configuration that capture covers is no longer checked at all — that is a coverage
decision, and it is the user's. Option 3 buys it by weakening the evidence. Option 2 is
correct and worth ~2 s. **Option 1 leaves all 678 s of recorded clock compared, byte for
byte, and only stops *waiting* for it.**

**And the honest caveat, before anyone acts on it**: option 1 is **not measured**, because
measuring it means changing the generator, which is the thing being decided. What is
measured is the three facts it rests on — the replays are **load-independent** (the suite's
clock moved 770,4 → 778,5 s under a machine at 54-71 %, and all of that is explained by the
settle and the load); the harness **already takes free ports per replay**, so the replays do
not collide; and the RAM headroom is large (**116,6 MB** against ~500 MB, and the peak is a
sum over at most 5 binaries, so concurrency multiplies the small part). **What is not known
is whether five concurrent replays hold the settle window** on a machine that is also running
the race detector. **That is the experiment to run before choosing it**, and it is not run
here.

**What is not recommended and why**: option 4 or 5 on their own. They fit the budget, and
they are the wrong trade — they are the only options on this list that make the suite
*less* faithful, and the budget exists to keep the suite runnable, not to make it cheap.

## 10. Against the original, on figures a client or an operator can see

Everything above in this file is a measurement of **our** code: our tick, our heap, our
profile. It is the right way to work and it has one blind spot that is worth naming: **there
was no comparison against the binary being reimplemented.** A Go tick can be made 42 % cheaper
and nobody reading the file learns whether the original spent more or less than that.

So: `src_go/cmd/benchpeer`. It launches **two binaries**, applies **identical measurement
rules** to both, and reports what each costs **as a running process**. It is a black-box
tool by construction — it knows a path, an argument list and a port, and it does not know
which of the two it is holding, so the same code produced both columns.

```
go build -o benchpeer ./cmd/benchpeer
benchpeer -bin ours.exe=ours,BattleServer.exe=original -window 15s -conns 8 -repeat 5
```

`-upgrades N` (default 5) is how many WebSocket handshakes each repetition takes, and it is
not a detail: see "What made the row unreadable" below.

### The figures, 2026-10-06, N=5 each

`[min-max]` is the range over the five repetitions, and it is not decoration: **two of these
figures are not readable as a single number** and the range is what says so.

| figure | ours | original | ours ÷ original |
|---|---|---|---|
| startup, port listening (ms) | **140,4** [131,6-144,6] | **81,5** [73,3-93,4] | **1,72** |
| websocket upgrade (ms) | **0,55** [0,46-1,10] | **129,4** [0,55-134,7] | **~120×** — and **the row is a distribution, not a number**: the original waits for its next tick, and our own value sits on this machine's clock noise floor. See below and `UNINTENTIONAL-DIVERGENCES.md` 12 |
| idle working set (MB) | **11,89** [11,61-11,96] | **10,05** [10,04-10,09] | **1,18** |
| idle commit charge (MB) | **46,84** [46,72-47,07] | **8,88** [8,88-8,93] | **5,28** |
| idle handles | **176** [174-176] | **118** [118-118] | **1,49** |
| idle process CPU (% of machine) | **0,04** [0,02-0,06] | **0,02** [0,01-0,03] | 2,0 (both ≈ 0) |
| system CPU over the window (%) | 23,93 [20,8-26,5] | 27,55 [25,3-39,8] | — |
| working set per held connection (MB) | **0,02** [0,01-0,02] | **0,02** [0,02-0,02] | **1,0** |
| commit per held connection (MB) | **0,03** [0,02-0,06] | **0,11** [0,11-0,11] | **0,27** |
| handles per held connection | **1,00** [1,00-1,25] | **1,00** [1,00-1,00] | **1,0** |
| threads per held connection | **n/a** | **n/a** | — |

### What the figures say, in order of how much they matter

**1. The idle commit charge is the one real cost, and it is ours: 46,8 MB against 8,9 MB,
5,3×.** It is also the most stable figure in the table (the range is 46,7-47,1, i.e. ±0,4 %),
so it is a property of the binary and not of the run. **An operator sees this number**, it
is the one place where the reimplementation is unambiguously more expensive than the
original, and it is the number to attack first if the footprint ever matters.

**What it is made of, measured rather than guessed.** This section used to say the 5,3× is
"the Go runtime's arena reservation and its GC metadata", and **that was wrong**, in the way
that matters: a reservation is not a cost. `src_go/cmd/vadbreak` walks the address space with
`VirtualQueryEx` and totals the **committed** bytes by region type, and on 2026-10-06 it
returned this, medians over three runs, with an empty Go program as the control:

| committed, by region | ours | an empty Go program | the original |
|---|--:|--:|--:|
| **`MEM_IMAGE` write-copy** — executable pages the loader dirtied | **32,55 MB** | 0,03 MB | **0,07 MB** |
| `MEM_PRIVATE` read-write — the heap and the runtime's own structures | 13,21 MB | 10,86 MB | 7,44 MB |
| `MEM_IMAGE` read-only, and execute-read | 25,80 MB | 12,50 MB | 27,10 MB |
| `MEM_MAPPED` — the DLLs | 4,83 MB | 1,57 MB | 6,57 MB |
| **commit charge**, the row above | **46,71 MB** | 11,70 MB | **8,42 MB** |
| `MEM_PRIVATE` **reserved** — address space | **5,25 GB → 0 committed** | 5,24 GB → 0 | 4,14 GB → 0 |

The two instruments reconcile: 13,21 MB private plus 32,55 MB of dirty image pages is
45,8 MB against a reported 46,71 MB, and the remainder is the writable image sections and
the private share of the mapped DLLs.

**Three things follow, and the first two are corrections.**

- **The arena reservation is not in this figure at all.** The runtime reserves **5,25 GB** of
  address space for the heap and commits **0 bytes** of it, and the original reserves 4,14 GB
  and commits 0 as well. It is address space on a 64-bit machine and it is free. On Windows
  64-bit this toolchain the arena is `1 << ((2+20))` = **4 MB**, a compile-time constant
  (`runtime/malloc.go:252`): there is no environment variable that changes it.
- **The 5,3× is 32,55 MB of dirty executable pages plus an 11,7 MB Go runtime floor.** Our
  own data is **2,35 MB** — 13,21 less the 10,86 MB an empty Go program already commits — so
  on the part that is data we are *cheaper* than the original's 7,44 MB. The earlier claim
  that the excess was "arena reservation and GC metadata" pointed at the one term that
  contributes nothing.
- **No collector setting moves it, and a manual GC provably cannot.** `GOGC=50` → 48,24 MB;
  `GOGC=20` → 48,53 MB; `GOMEMLIMIT` at 32, 16 and 8 MiB → 46,96, 47,36 and 47,79 MB;
  `-ldflags="-s -w"` (10,67 → 7,39 MB on disk) → 46,31 MB; `GOMAXPROCS` 1/2/12 → 45,22 /
  45,79 / 46,80 MB. Those are the **only two** memory variables the Go runtime reads at all.
  With `GOGC=1 GOMEMLIMIT=8MiB` the managed heap is **0 MB**, two GC cycles fire in 15 s, and
  the commit charge *rises* to 47,79 MB: there is nothing to collect, so a `runtime.GC()`
  after start-up would free nothing. `GODEBUG=inittrace=1` attributes **99 KB across 97
  packages** to our `init()` functions, so it is not ours either — it is the runtime's and
  the loader's.

**The one term Go offers no lever for is the 32,55 MB, and it has an address.** It is 3× the
size of the binary that carries it, so it is not file-backed content, and it is not "the
writable image sections, dirtied by the loader and by startup" in general: it is **one
symbol**. `go tool nm -size` on this tree, sorted by size, puts it first by two orders of
magnitude:

```text
140766d40   33554432 B crypto/internal/fips140/drbg.memory
140390f00    2192368 R runtime.pclntab
14061ce68     658704 R type:*
```

`crypto/internal/fips140/drbg.memory` is a **32 MiB `ScratchBuffer`** for Go's CPU-jitter
entropy source (`crypto/internal/entropy/v1.0.0`: `type ScratchBuffer [1 << 25]byte`). It is
linked whenever `crypto/tls` is linked, which this server always does: the out-of-band port is
WebSocket-over-TLS and the WebSocket port may be TLS. On ELF the buffer is lazily backed; **on
Windows the PE loader commits the whole `.data` virtual size**, and the `.data` section is
**32,66 MB of virtual size against 0,32 MB of raw size**. The 32,5 MB is that zero-fill, and
`vadbreak` attributes it exactly: `MEM_IMAGE READWRITE 32,53 MB` over 16 regions, which is the
row in the table above.

**It is not resident.** The working set `benchpeer` measures is **11,9 MB**, so this is a
commit-charge figure and not a RAM figure, and the working set is a tie with the original
(11,9 against 10,1 MB). That distinction is the whole of the entry: an operator watching
working set sees a tie, and one watching commit charge sees 5,3×.

**What does not remove it, measured or checked:** `-ldflags="-s -w"` (46,71 → 46,31 MB),
`GOGC`/`GOMEMLIMIT` (no effect, above), `-buildmode=pie` (the symbol is still present),
`GOFIPS140=off` (already the default, and the buffer is still linked), and Go 1.26.5 (the same
32 MiB symbol). Go 1.25.5 does not have it — its `.data` virtual size is **0,48 MB** and
`go tool nm` finds no `drbg.memory` — but pinning 1.25 would drop the Go 1.27 features this
tree uses, so it is not proposed. A `notls` build variant would remove it and would also
remove WebSocket TLS and the out-of-band port; that is a **feature** decision with its own
release note, not a performance one, and it is not taken here.

It is also the term a port would remove, which is what makes the footprint case for one a case
about **the runtime** and not about the server.


**2. Idle working set is nearly a tie: 11,9 MB against 10,1 MB.** The gap is 1,8 MB and it is
*not* the same 38 MB as the commit charge, because a Go process's working set is what its
runtime has actually touched, not what it reserved. **And it is only stable because the tool
repeats**: an early single run of this same comparison reported **51,7 MB** for ours, 4,3×
the median, and a single-run tool would have printed that and called it the figure.

**3. The per-connection cost is a tie, and on commit we are the cheaper one: 0,03 MB against
0,11 MB per held connection.** This is the figure that answers a question §5 could not: §5
measures what a connection costs *us* and has nothing to compare it against. Against the
original it is a wash on working set and handles and 3,7× better on commit, so **the
connection path is not where a reimplementation costs more than the thing it replaces.**

**4. Idle CPU is negligible for both, and that is the point.** 0,04 % and 0,02 % of the
machine. Neither server spends anything measurable while nobody is playing, so the tick's
cost (§3, 265-319) is entirely tick-bound work and neither implementation is paying for idle
polling. The 1,3 s animation loop discussed in §8 does not show up here as CPU, because on
this machine it does not.

**5. Startup costs us 1,7×: 140 ms against 81 ms.** Worth recording and not worth optimising:
it is one launch, it is well inside anything a launcher waits for, and no operator has ever
complained about it. It is in this file because it is a figure, not because it is a problem.

### What this section does NOT establish

- **It is not a comparison of algorithmic efficiency.** It is a comparison of what the two
  **processes** cost, which includes each language runtime's idle footprint and garbage
  collector headroom. Ours is 5,3× on commit, and the decomposition above says where that
  is: **32,55 MB of dirty executable pages, which is one 32 MiB entropy buffer the Windows
  loader commits**, and an **11,7 MB Go runtime floor**, against our own **2,35 MB** of data.
  That is a runtime property and not a defect in the server, and no amount of work in
  `internal/session` would change it — which is also why the collector settings do not, and
  why they were tried before the claim was rewritten.
- **The handshake row used to say "not comparable" and that was wrong. It is now measured,
  and it is a divergence.** It said "the original is bimodal… a first-connection cold path
  **on our side** is the obvious candidate and it was **not isolated**". Isolated on
  2026-10-06, and the cold path is not ours. **The original defers the WebSocket upgrade to
  its next main-loop tick**; ours is served on the connection's read path. The prediction that
  the deferral follows the period was tested by moving it, and it does:

  | `-simulationPeriod` | ours, median | original, median |
  |---|--:|--:|
  | 30 ms | 1,12 ms | 36,9 ms |
  | 125 ms (default) | 1,12 ms | 128,2 ms |
  | 500 ms | 1,09 ms | 513,3 ms |

  **Ours does not move; the original's does.** The work itself is 0,5-15 ms on both sides, and
  the original's 15 samples are two clusters with an empty gap between them
  (`0.54 0.95 1.14 1.97 3.14 │ 124.90 … 135.84`), so the extra time is a whole number of
  periods and not a slow path. **No byte differs** — a client can only time it. It is filed as
  an open entry with no written reason in
  [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 12, and the criterion half it
  does not settle is named there.
- **What made the row unreadable is the tool, not either server.** `benchpeer` used to take
  **one** upgrade sample per repetition and report the **median of a bimodal distribution**, so
  the number in the table depended on the tick phase and on N, and **at N=5 the row can
  invert**. It now takes **`-upgrades N` handshakes per repetition (default 5)** and **prints
  the samples** in microseconds beside the figure, because a median of a bimodal set says which
  cluster won and not what happened.
- **A fifth fact, and it bounds every sub-millisecond figure in this file: this machine's
  monotonic clock is coarser than the thing it is being used to measure.** A standalone probe
  of **200 loopback TCP round trips** returns **zero** for **94 of them (47%)**, with a median
  of **509 µs** over the rest. A reading of `0.00 ms` on the upgrade row is therefore **a
  clock that could not resolve it, not a fast handshake**, and the tool now **counts those
  samples instead of carrying them in as zeros** and reports them as `(+N the clock could not
  resolve)`. It is also why the samples are printed in **microseconds**: stored in
  milliseconds and printed to two decimals, every reading under 10 µs becomes the same
  `0.00`, indistinguishable from a reading that never happened.
  **What this does and does not invalidate**: `startup` at 140 ms stands. **Our own upgrade
  figure does not** — the samples are 441-1210 µs, so **0,55 ms sits right on the noise
  floor** and must not be quoted as precise. The comparison survives, because the original's
  125 ms is two and a half orders above that floor and ours is not close to it, and because
  the claim rests on the `-simulationPeriod` sweep rather than on the absolute value. **The
  honest sentence about our side is "around a millisecond, and it does not move with the
  period", not "0,55 ms".**
- **Thread count could not be measured on this machine** and is `n/a`, not zero. See below.
- **Nothing here measures a match.** Both columns are servers holding an open port with no
  clients. The figures that would need a real game are §3 (our tick, with its sweep) and §5
  (our heap curve), and **neither has an original counterpart yet** — that is the gap this
  section closes halfway and names.

### Why the tool reports "n/a" instead of 0

`Thread32First` returns `ERROR_BAD_LENGTH` on this machine for a `THREADENTRY32` whose
`dwSize` is the correct **48 bytes**, for every `TH32CS_SNAPTHREAD` flag combination tried.
The struct size is not the variable: it was checked against a standalone probe before this
was called a limitation. The figure is therefore left out, because **a zero thread count is
not a reading either of these binaries can produce** and reporting one would be exactly the
failure §2 documents — a filter or an API that matches nothing, reporting a zero as
confidently as the thing it measures.

Two API facts had to be measured to get the rest, and both are written into the source:

- the memory counters must be read with `kernel32.dll!K32GetProcessMemoryInfo`, because the
  same-named `psapi.dll` export returns `ERROR_BAD_LENGTH` for **every** `cb` value including
  the correct one;
- `cb` must be the size of the **plain** `PROCESS_MEMORY_COUNTERS` (72 bytes), not the `_Ex`
  one (80), which the plain call also refuses.

Neither failure is visible as a wrong number: both return a zeroed structure. That is why
`benchpeer` carries an error out of every counter read and prints it as `UNAVAILABLE`, and it
is the same reason §2 exists.

### A third fact, found by `vadbreak`, and it renames the column

**The row this file has been calling "idle commit / private" is `PagefileUsage`, the commit
charge. It is not `PrivateUsage`.** The consequence of the second bullet above is that
`PrivateUsage` **cannot** be read: it lives in the `_Ex` structure, the plain call refuses the
80-byte `cb`, and `benchpeer` uses 72. The 46,8 MB figure is therefore the process's commit
charge, which is the right number for "what does the OS have committed for this process" and is
now labelled that way in the table above.

This was found by writing the second instrument and asking it to print the same figure, because
two tools that measure the same thing must be reconcilable. The first version of `vadbreak`
summed `QuotaPagedPoolUsage + QuotaNonPagedPoolUsage` — two *quota* fields, a third figure
again — and reported **0,16 MB** for a process whose commit charge is 46,71 MB. It looked like
a measurement because it was a real counter read. `vadbreak` now reads `PagefileUsage` and
`TestCommitChargeIsTheFigureBenchpeerReports` rejects a value under 8 MB for exactly that
reason.

**A fourth fact, and it is the one that silently produced wrong attributions.**
`SYSTEM_INFO.dwPageSize` is at **offset 4**, after two `WORD`s. Read through a pointer-sized
field it lands on `lpMinimumApplicationAddress`, which on 64-bit Windows is `0x10000`, so the
walk stepped over **64 KB** at a time wherever `VirtualQueryEx` declined to describe a region
and the attribution came out wrong while every total still printed. This produced a
contradiction in this repository's own numbers — the same binary measured at **32,55 MB** of
dirty image pages and at **0,29 MB** on the same machine — and the totals reconciling is what
made the difference visible rather than a matter of opinion.
`TestPageSizeIsReadFromTheRightOffset` now pins it.

### The second instrument, and what it is for

```
go build -o vadbreak ./cmd/vadbreak
vadbreak -pid 12345            # a table
vadbreak -pid 12345 -json      # the same figures, machine-readable
vadbreak                       # walks this process, which is what its own test does
```

`benchpeer` answers **how much**. `vadbreak` answers **what of**. It carries the same three
rules as the first tool, and the second one is the one it was written for: **a figure the
walk could not read is absent, never 0**, and on a platform other than Windows it exits 2
with `UNAVAILABLE` rather than printing a table of zeroes. It duplicates the 72-byte
`processMemoryCounters` layout and the `kernel32` choice from `cmd/benchpeer/procinfo_windows.go`,
and both files say so, the same way `cmd/proxy/identity.go` and `scripts/deploy.ps1` carry the
same two marker lists.

## Where each measurement is

| what | file | section |
|---|---|---|
| the budget and why | this file, section 0 | `SESSION-JOURNAL.md §25` (end) |
| against the original, black box | this file, section 10 | `src_go/cmd/benchpeer`, measured 2026-10-06 |
| where the commit charge is | this file, section 10 | `src_go/cmd/vadbreak`, measured 2026-10-06, and `SESSION-JOURNAL.md §49` |
| the suite's measurement | this file, section 1 | `SESSION-JOURNAL.md §36.5`, `§36.6`, `§38.5`, `§39.8`, `§40.5`, and the 42's check |
| the sampler and its three failures | this file, section 2 | `SESSION-JOURNAL.md §36.6`, `§39.8`, `§40.5` |
| **the suite being over budget, and the options** | **this file, section 9** | `SESSION-JOURNAL.md` **§45.4**, `trimsim -inspect` over the five committed captures and the one on disk |
| what a tick costs | `internal/session/tickcost_test.go` | `SESSION-JOURNAL.md §38.1`, `§38.2`, `§40.2`, and the 42's measurement |
| the per-stage distribution | `internal/session/tickcost_test.go` (`TestTickCostSplitsIntoStages`) | `SESSION-JOURNAL.md §39.7`, and the 42's measurement (which declares `NOT RESOLVED`) |
| the tick's profile | `internal/session/tickprofile_test.go` | `SESSION-JOURNAL.md §39.1` |
| the server's profile | `internal/server/serverprofile_test.go` | `SESSION-JOURNAL.md §39.1`, `§39.4` |
| the three `AckState` maps | `internal/session/ack.go` | `SESSION-JOURNAL.md §39.3` |
| the peers table | `internal/session/session.go` (`peerSlots`, `peerOrder`) | `SESSION-JOURNAL.md §40.2` |
| short and long memory | `internal/session/ack.go`, `replay.go` | `SESSION-JOURNAL.md §39.2` |
| the long match | `internal/session/longmatch_test.go` | `SESSION-JOURNAL.md §34` |
| the client sweep | `internal/session/peersweep_test.go` | `SESSION-JOURNAL.md §37.2`, `§37.3` (table from before §38.3) |
| the upload queues | `internal/session/peerqueue_test.go` | `SESSION-JOURNAL.md §37.4`, `§37.7` |
| the cost of a connection | `internal/server/conncost_test.go` | `SESSION-JOURNAL.md §37.6` |
| goroutines and leaks | `internal/server/goroutineleak_test.go` | `SESSION-JOURNAL.md §35` |
| reading a pprof | `internal/profiletest/` | `SESSION-JOURNAL.md §39.5` |
