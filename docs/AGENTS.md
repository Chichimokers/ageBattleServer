# AGENTS.md

Read this **before** touching anything. It is here because the history of this session
lived in the conversation and conversations get lost; everything you need to know is in
the repository.

## TWO QUESTIONS THAT ARE CLOSED. THEY DO NOT REOPEN.

- **The BattleServer is deterministic.** It is the one shipped with Age of Empires, so the
  original's output is reproducible. Closed **by authority**: it is not measured, and no
  run that would contradict it is looked for. What does keep being done is comparing
  against the recording, which is the available copy of that output.
- **The observer's excessive zoom is not a BattleServer bug.** Confirmed from outside.
  Section 25 of `SESSION-JOURNAL.md` already measured it and closed what could be closed:
  the observer's stream is indistinguishable from the original's, byte for byte.
  Investigation stops there. What stayed open in it (ack grouping, `LobbyConnect`, a ping
  where the other answers with a pong) is informative, and **the `LobbyConnect` one is not
  the zoom**: it is a transport divergence and is still alive, in
  `UNINTENTIONAL-DIVERGENCES.md` 4. The "ping" was **never a ping** — it was two bytes of
  the `101` response's second TCP record, the `e` of `Sec-WebSocket-Accept` against the
  `k` of the accept key — and it was closed as a false claim on 2026-10-05
  (`SESSION-JOURNAL.md` §44.3).

The long text of the two, with their evidence and with what stays open inside each one,
is in **[`CLOSED-QUESTIONS.md`](CLOSED-QUESTIONS.md)**. This section here is the
authoritative summary; that file is the one you read when you need to know *why* they are
closed.

## What is here

## What this is

A reimplementation of `BattleServer.exe` (AoE2DE / AoE3DE) in Go, compared byte for
byte against the original binary decompiled in `src/`.

- `src_go/` — the reimplementation (Go submodule, with its own commits)
- `src/` — decompilations of the original, with addresses
- `captures/` — new captures, one folder per run. Ignored by git
- `headers.hpp` — live include of the decompilation: 7,450 lines out of the
  7,450 files in `src/` include it with `#include "../headers.hpp"`
- `BattleServerOrig.exe.pristine` — the untouched original, for the identity
  comparison
- `battle-server.txt` — the console output of the **original** (2026-08-11). It is the
  only runtime measurement of its 1.3 s animation loop

# THE DOCUMENTATION: WHERE EVERYTHING IS

The documentation is split by subject. **The current design is living files, one per
subject**; the journal is history and it is where the evidence lives. The complete map,
with the verdict on each file in the root and the reason for each one, is in the table
below; why it is that way in **[`SESSION-JOURNAL.md` §41](SESSION-JOURNAL.md)**
(the split) and **§36.4** (the audit that preceded it).

### The living files, by subject

| file | what it is about | status |
|--|--|--|
| `AGENTS.md` | **the front door**: the rules, the harness, the deployment | current |
| [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) | **the differences we chose and that are visible from outside**, each with its reason and its evidence, and the list of what was retired for not being observable | current |
| [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) | **the open bugs and the unexplained gaps**, the `UNKNOWN`s that remain, and the questions with no answer. Its **section 0 is the criterion applied to every entry**, with what each one passed or failed it on | current |
| [`PENDING.md`](PENDING.md) | what is missing, split by who can resolve each thing | current |
| [`CLOSED-QUESTIONS.md`](CLOSED-QUESTIONS.md) | the two closed questions, with their evidence | current |
| [`PERFORMANCE.md`](PERFORMANCE.md) | CPU, RAM, allocations, goroutines, methodology and numbers, and what was left untouched | current |
| [`CAPTURES.md`](CAPTURES.md) | how to record a capture, its matrix, **the results one by one**, and the ceiling of what the harness can ask | current |
| [`PROVENANCE.md`](PROVENANCE.md) | **what this repository is and what may not be redistributed**: the per-path list, the statement that this is not a clean-room implementation, and what was measured. **It is itself on the do-not-distribute list** | current |
| [`BEHAVIOUR-NOTES.md`](BEHAVIOUR-NOTES.md) | **the recovered structure written as behaviour**: 22 sections, no decompiler symbol in any of them. What the Go comments no longer say | current |

The two divergence files are **mutually exclusive**, and the criterion has two halves, and
both halves have to be applied:

1. **That it is visible from outside.** A difference is a divergence if a client, a peer or
   the replay host can tell it apart **on the wire**. If the only thing that changes is how
   the code is organised inside —a map against a slot table, a pooled buffer against a
   reserved one, a cheaper algorithm with the same output— **it is not a divergence**: it
   is a design decision, and it lives in [`PERFORMANCE.md`](PERFORMANCE.md) if it is about
   cost and in [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) if it is about why it was decided.
   Section 7 of `INTENTIONAL-DIVERGENCES.md` lists what was retired for this reason.
2. **If it is visible from outside, why do we accept it?** If there is a written reason it
   is intentional; if there is not, it is a bug. A "do not touch" with no written reason
   does not count as a reason, and a question with no answer does not count as a reason
   either: that one goes in the unintentional file as an `UNKNOWN`.

### The description of the original, and the rest

| | what it is about |
|--|--|
| `bsPort.md` | the game port: flags, RLink framing, 27012 |
| `webSocketPort.md` | the WebSocket port: RFC 6455 (NOT Hixie-76), handshake |
| `MESSAGES.md` | the table of message types, the source of the numbers |
| `oob.md` | the out-of-band port |
| `LOBBY-PROTOCOL.md` | the heartbeat with the lobby |
| `replays.md` | the original's replay upload over HTTP POST (not implemented here) |
| `uploadVia-lobby.md`, `uploadVia-lobby-connection.md`, `uploadVia-lobby-state.md` | the three steps of the original's `-uploadVia=lobby` (the flags are accepted and ignored here) |
| `go1.27.md` | a copy of the Go 1.27 release notes. Not a project document, but §35 and §39.5 cite it |
| `relay.md` | the presence beacon (`-relayBroadcastPort`). **Verified against `src/`: it is original behaviour**, and it now also carries what we do differently from the original |
| `README.md` | the general index. Rewritten in English and checked against the tree; it is an index and nothing else |

### Superseded: three deleted, one kept

Four documents were superseded. `README.md` has been rewritten and is no longer a file that
cannot be touched, so the reason they were kept — "they are linked from `README.md`, which is
the parent's work" — no longer applies, and each was decided on its own:

- **Deleted, `uploadVia-mysql.md` and `uploadVia-odbc.md`.** At the time the server
  **rejected** `mysql` and `odbc` with `os.Exit(1)`. A document describing a supported method
  that the server refuses is worse than none. The six links that named them (in the three
  `uploadVia-lobby*` files and `LOBBY-PROTOCOL.md`) are gone. **Update 2026-11-03: the replay
  upload has been removed from this implementation entirely**, so the flags now accept any
  value and are ignored; the history above is why the two documents are still not here.
- **Deleted, `PROXY-CAPTURE-VALIDATION.md`.** It described a capture format that no longer
  exists (`main_log_*.csv`); the layout is `CAPTURES.md` and the validation is `internal/sim`.
  Its measurements were **not** lost with it — they are in `SESSION-JOURNAL.md:308-312` (§`_4`:
  the game died at 294.2 s with both servers, relays 202/202, ping echoes 71/71). The one link
  into it, `webSocketPort.md:55`, now cites `CAPTURES.md` and the journal.
- **Kept, `WEBSOCKET-BSPORT-COMPARISON.md`.** Its conclusion ("the two ports are
  equivalent") is refuted by §27: in bsPort you cannot observe, and the TLS belongs to the
  port. But its **packaging, handshake, socket-manager and framing detail still holds** and no
  other document carries it, so deleting it would lose real content. It keeps its banner and
  is listed in `README.md` as the superseded-but-kept one.

Nothing that names them as a file to read is left pointing at a file that is gone; the
journal mentions all three **by name in backticks**, as history, and its mentions are not
links.

### The journal

`SESSION-JOURNAL.md` is **7,118 lines and 450 KB of history** (it was 6,659 lines and
415 KB in Spanish, before the translation): the rounds, with their
measurements and their falsifications in the place where they were produced. It carries an
index at the top that maps **section → topic → living file**, so to find "where the byte
of the 0x19 is written down" the index is enough. **Read it last**, and by sections.


## Hard rules

1. **Only what is demonstrated.** No "it should work". Every claim about the original's
   behaviour comes from `src/sub_*.cpp` or from a capture. If it is in neither of the
   two, it is not touched.
2. **Do not touch the T1/T2 forwarding.** Instrumentation only. The proxy does not alter
   the traffic, it only records it.
3. **`webSocketPort = 0`.** Always.
4. **GPG signed commits** (`git commit -S`). If the key has expired, retry when the user
   authorises the pinentry. Never make unsigned commits and never use `--no-gpg-sign`.
5. **Do not touch the parent's changes**: `scripts/fidelity.py`, and the deletion of
   `build/BattleServerReimpl.exe` and `docs/architecture_refactor.md`. **`README.md` and
   `relay.md` are no longer in this list**: the user lifted that protection for those two on
   2026-10-05 and authorised their rewrite, and both have been rewritten and verified. They
   are ordinary documents now. **`cloc-2.10.exe` and `gsa.exe` are no longer in this list
   either**: the user deleted both on 2026-10-07 and asked for what referred to them to go
   with them, so the rule no longer protects files that are not in the tree. Two Windows
   binaries, 33 MB, neither of them ours nor used by anything here.
6. **`src_go/` states the wire contract, not the binary's internals** (added 2026-10-06,
   `SESSION-JOURNAL.md` §47). No comment in `src_go` may carry a decompiler symbol
   (`sub_1400XXXXXXXX`), a recovered vtable slot (`PeerMessage__vmethod_18`), a recovered
   class method (`Session::Tick`), narration of the original's memory layout
   (`session+2192`, `peer+0x5A`), decompiled pseudocode, or a log string taken verbatim out
   of the original binary. **The measured count of each is 0** and the check is a regex over
   `src_go/**/*.go`; it is not a convention, it is a number. What a comment **must** do is
   state the required behaviour, and the finding behind a decision goes in
   [`BEHAVIOUR-NOTES.md`](BEHAVIOUR-NOTES.md) or in `SESSION-JOURNAL.md`, which are the
   research side and are where the citations live. **Rule 1 is unchanged and rule 6 does not
   weaken it**: a claim about the original's behaviour still comes from `src/sub_*.cpp` or a
   capture, and the citation that says which one moves to the journal, not away.
7. **Do not distribute the owner's material** (`PROVENANCE.md`). The executable, `src/`,
   `headers.hpp`, `battle-server.txt`, `captures/**`, the committed capture fixtures and
   `observer_vectors.json` are the copyright owner's, or recordings of it. They stay in
   this repository for the research and they do not go into a published one.
   **`PROVENANCE.md` itself is on that list, and so are `SESSION-JOURNAL.md` and
   `BEHAVIOUR-NOTES.md`.** The first says in its own words that the implementation was
   informed by disassembling the original; the other two carry the decompilation's
   citations by design. Publishing any of the three converts a defensible position into
   an acknowledged derivative-work claim, in the project's own words. **Honest internal
   records and voluntary public admissions are different acts and only the first is
   free.** Nothing is staged for distribution and nothing has been distributed.
8. **Always verify before saying something is there**:
   ```
   CGO_ENABLED=0 go build ./...
   CGO_ENABLED=0 go vet ./...
   CGO_ENABLED=0 go test ./... -count=1
   CGO_ENABLED=1 go test -race ./internal/server/ ./internal/session/ -count=1
   ```
9. **RAM and time budget for the tests.** A test that brings the machine down is a
   broken test even if it passes:
   - Nothing generates synthetic data without an explicit cap: if you synthesise a match,
     the number of events carries a limit in the code itself, not in someone's head.
   - No O(n²)-memory algorithms over real captures. The harness diff **resynchronises** at
     every step, which is exactly what makes it explosive. Measure the consumption, do not
     assume it: run the package watching the working set. A test that goes past a few
     hundred MB is treated as a failure, and it is said out loud before launching it if it
     does not fit.
   - Suite budget: **10 minutes and ~500 MB**. If something does not fit, it is trimmed,
     the ceiling is not raised.
   - **The budget is for the COMPLETE suite.** The short loop has its own (~45 s) and it is
     separate, not a share. See "THE TWO LOOPS" below.
10. **Commit BEFORE moving or renaming files.** An hour of work was lost in this session by
   moving a file without committing it. No exceptions.
11. **Nothing half-done in the tree.** A file left unverified in the tree is an accident
   waiting for `go test ./...`: it is either committed or thrown away, but never left in
   between. Temporary test files go outside the tree.

## Identity of the binaries: by content, never by name

`BattleServerOrig.exe` has contained our build and the original, and `BattleServerReimpl.exe`
the other way round, on different dates. The names mean nothing.

- **Ours** = Go binary (markers `Go build ID:`, `runtime.main`, `go1.`).
- **Original** = MSVC markers.
- **Unknown** = neither one nor the other, and then nothing is concluded.

`cmd/proxy/identity.go` decides it, and it also writes `roles.json` and `COMMIT.txt`
into every capture before launching anything. Discarded as a fingerprint:
`Server::Start`, which is generated at runtime and is in the bytes of no binary.

## Capture layout

The proxy creates `./captures/<date>_<commit>/` **relative to the working directory**,
and **deliberately ignores `MAIN_LOG_PATH` / `SECONDARY_LOG_PATH`**:

```
main.log  secondary.log  proxy.log  metadata.json  COMMIT.txt
main.meta.json  { "file": "main.log", "writer": "original" }
secondary.meta.json
```

`writer` is a **tri-state** (`ours` / `original` / `unknown`), not a boolean: "it is the
original" and "it could not be determined" are different facts, and with a bool an
unidentified capture would look decided.

`metadata.json` carries `commit`, `dirty`, the `writer` of each side, the sha256 of each
binary, and **`primary_args` / `secondary_args`**: the exact command line each side was
started with. Without that you cannot reconstruct the server.

## Deployment

```
.\scripts\deploy.ps1            # main = ORIGINAL, secondary = OURS (default)
.\scripts\deploy.ps1 -Swap      # main = OURS, secondary = ORIGINAL (comparison captures)
.\scripts\deploy.ps1 -Revert    # restores the timestamped copy
.\scripts\deploy.ps1 -WhatIf    # dry run
```

It verifies by content what it leaves on disk and fails if it is not what was asked for.
It compiles with `-ldflags` to stamp the commit. **The tree must be clean** for
`dirty=false`.

## The language check

**File names and content, all in English, everywhere.** That is a claim, so it has a
command:

```
python scripts/check_language.py --strict    # exit 0 means it holds
```

It reports every remaining Spanish occurrence in this tree's documentation and in
`src_go/` — **with file and line**, and **the file names as well as their content** — and
it exits non-zero when it finds any. `--quiet` prints only the counts, `--selftest` runs
its own detection and false-positive tests, `--list-allow` prints the six exemptions with
the reason each one exists (there is no silent suppression path: an exemption is a line of
`ALLOWED` in the script, with its reason next to it, where a reviewer sees it).

It does not check `src/`, `captures/`, `build/` or `go1.27.md`, and inside a file it
checks **prose only**: not fenced code, inline code, link targets, URLs, hex literals,
paths or identifiers. **A commit that adds Spanish fails this**; so does a commit that
renames a file to a Spanish word, which is the check that would have caught the two names
the 43 missed (`SESSION-JOURNAL.md` §46). Its measured false-positive rate on 3,322 lines
of English prose is **zero**, and it reports 246 real hits when run against the tree as it
was before §46.2.

---

# SIMULATION HARNESS: DONE

`src_go/internal/sim/` + `src_go/cmd/gensim/`. A capture is a test, and the test knows
nothing about the capture. Adding a capture is leaving a directory behind.

```
captures/2026-09-28_040213_56c1309/     what the proxy writes
        |
        |  cmd/gensim          (or: go run ./cmd/gensim captures)
        v
src_go/internal/sim/testdata/<capture>/
src_go/internal/sim/sim_<capture>_test.go   generated, one function:
        func TestSim_<capture>(t *testing.T) { sim.Run(t, "testdata/<capture>") }
```

`gensim -check` writes nothing and fails if a generated test does not correspond to what
the captures would produce. It is what runs in continuous integration, and
`TestGeneratedTestForEveryCapture` does the same inside `go test`, in both modes: a
capture cannot be added without its test even if nobody remembers to run the `-check`.

## THE TWO LOOPS

A capture is a test that costs what the recording cost, because the replay runs in real
time on purpose (`Options.ReplaySpeed`: the server emits `0x0A` per tick, not per input).
Five captures are ~760 s of wall clock, and paying that on every change is what stops
anyone from running the suite.

The cut is by **recorded clock**, not by capture name, with the budget as a constant in
the code (`src_go/internal/sim/short.go`):

```
go test -short ./...          the short loop: 41 s. Long captures are skipped
go test ./... -count=1        the full suite: 762 s, or the CI command
```

`ShortModeBudget = 20s`. The synthetic fixture records ~1 s and the shortest real capture
47 s, so the short loop is "the capture that needs no capture, plus everything that is not
a capture". A list of capture names goes stale the day a capture is added: either it drops
a capture out of the short loop by accident, or it keeps one in forever.

**A green short loop is not a green suite.** What holds that up:

- `TestMain` prints the MODE before starting, and every long capture is skipped with a
  `Skipf` that carries its recorded clock and the command that replays it.
- `TestFastPathIsNotTheFullSuite` **fails** if the short loop leaves out the same thing the
  full loop does, and if the classification depends on the mode: a list that moved with the
  mode would be a rule choosing what to **run** instead of what to **skip**, which is how a
  capture falls out of the short loop without anyone choosing it.
- The synthetic fixture has to be in the short loop: it is the only coverage that needs no
  capture, and it is the one that has found bugs no replay has found.

An honest caveat, measured: a `go test` of a package that passes **prints nothing** of what
the test binary wrote (checked on this toolchain with a test that prints through stdout,
through `t.Log` and through stderr). Without `-v` all that is left is the time on the `ok`
line, 41 s against 768 s. With `-v` or `-json` it is all there.

## What the harness decides

- **The truth is always the original**, and `metadata.json` says which of the two logs it
  is (`primary.writer` / `secondary.writer`), never the file name. `main.log` and
  `secondary.log` are fixed names and the two binaries have been swapped from run to run. A
  capture with an unidentified side is **rejected**, not passed: a tri-state that collapses
  to "fine" hides the one fact that makes it unusable. Both sides being `original` is also
  rejected, and so is a `*.meta.json` that contradicts `metadata.json`.
- **The configuration is the recorded one.** `primary_args` / `secondary_args` are parsed
  with `config.ParseFlags`, the same parser `cmd/battleserver` uses, and with **our
  side's args**, which is the configuration that has to be reproduced. The original's are
  printed alongside and their difference with ours is reported: if the two sides were not
  launched with the same thing, a divergence that comes out may be of the flags and not of
  the protocol.
  `ParseFlags` reads `flag.CommandLine` and `os.Args`, which are process-wide, and calls
  `os.Exit` on three paths (unknown flag or bad value, `-help`, and the usage check on
  `-simulationPeriod` 0 together with an observer interval flag),
  and a `FlagSet` with `ContinueOnError` **contains none of them**: the exits are inside
  the function. That is why parsing is done in three steps: registration with a call with
  no arguments (which cannot exit), validation over that same `FlagSet` (which does return
  the error), and only then the final call. A flag the parser does not know is dropped and
  reported, not guessed at.
- **Only what is not ours is changed, and it is reported.** The listen ports go to free
  ports (the capture's are the ones of the proxy that made it, and no compared byte carries
  a port), the out-of-band is turned off (it is another service and no battle message goes
  through it), the lobby is turned off (it does not exist in a test) and the certificate is
  discarded if the file is not here.
  `maxPagesPerMatch`, `-simulationPeriod` and the rest are reproduced as they were, because
  they decide whether the observer gets in at all. Every change comes out in the report
  with its reason.
- **Byte-for-byte comparison** of the message, with a single masked field: the estimated
  start time of `0x1E`, which comes from the clock. The list of masked fields goes in the
  report, so the list can be argued about.

## The two levels of verdict

**must-match**, what the client depends on:

- the byte of each `0x19` StartObserveResponse
- whether `0x1E` ActivateObserverResponse arrives, and its status byte
- the replay stream the observer receives (`0x0A`, the same one the player gets, because
  that is what the frame store keeps) and at what point of the match it starts: the order
  and the first frame. The `0x09` is the player's, not the observer's: reading it there
  left the rule comparing two empties.

**informative**, counted and printed with both sides' frames side by side, but it does not
fail: the `0x0A`s are built per simulation tick, so a compressed replay groups differently
without being wrong.

The number of frames is NOT must-match; the order and the starting point are. An inverted
pair crashes the client (capture _5); how many fit in a tick says nothing about fidelity.

## What is left

**The complete table of results, capture by capture, is in
[`CAPTURES.md`](CAPTURES.md)**, which is the reference: each row is a capture committed in
`src_go/internal/sim/testdata/` with its MUST-MATCH, its clock and what it contributed. What
comes out of the history before it was deleted, and is also in `CAPTURES.md` and in
`src_go/internal/server/testdata/observer_vectors.json`:

| capture | side | `0x19` | `0x1E` | pages |
|--|--|--|--|--|
| _26 | original | **0** | **yes** | **7343** |
| _28 | ours | 3 | no | 280 |
| _29 | ours | 3 | no | 379 |
| _30 | ours | 3 | no | 390 |
| 2026-09-29_012528 | **ours at the front** | 0 (2 times) | yes | 1359 and **603**, against 1353 and **803** from the original |

The only `0` is the only one followed by `0x1E` and the only one with thousands of pages. A
`3` means the client never gets to activate and then says there is no data from the start.
And the `0` does not mean "there is no replay": it is the arm that **registers** the
observer, and the `3` is the one that does not register it because the store has already
lost the beginning of the match. See SESSION-JOURNAL.md 22 and 24. Covered by
`TestStartObserveMatchesCapturedClientRequest`, with _26's real bytes, verified falsifiable.

The `0x19` byte has **four values** in the binary and each one goes on the wire as it is:
`3` (`sub_1400E3B50:93` and `:113`), `0` (`:170` and `:219`), `2` (`:295` and `:344`) and `4`
(`:403` and `:452`). **The `0` is the only one that is an acceptance and not a rejection**:
it is the arm that registers the observer (`:237-254`), which is why the only one is the only
one followed by `0x1E`. The `3` is the store already cycled, the `2` is the observer list
full and the `4` is the observers deactivated for the session. Until 2026-10-01 we sent the
`3` as `4`, which is a different rejection. None of the
five committed captures had a store that had cycled a block, so there was no capture to
tell them apart: `testdata/synthetic-rollover` found it, which is the SAME capture with
`-pageSize 128` and a single block. See SESSION-JOURNAL.md 28.

Pending — **the complete count, with what each one needs, is in
[`PENDING.md`](PENDING.md)**. The minimum:
- **There are already five real committed captures**: `2026-09-28_205830` (**original** at
  the front, no observer), `2026-09-29_012528` (ours at the front, with two observe
  attempts), `2026-09-30_010531` and `2026-09-30_183120` (both with an observer) and
  `2026-09-30_201122` (**bsPort**, no observer, the first of that transport). All five with
  **MUST-MATCH 0**. The 9 of the `CAPTURES.md` matrix are still pending; start with
  **row 1 (AoE2DE, full match, original at the front)**.
- The harness has now been seen against **five** real captures, and the second is the one
  that uncovered the frame store. The direction of each row is decided by the port of the
  recorded line, and that was checked with 012528.
- **"We tick differently" is closed**: it is 4 extra acks in second 26, and they come from
  the replay, not from the server. In the run the capture itself recorded, the original put
  3 in that second and ours 2, with 8 and 8 in each of the 62 following ones and the same
  500 in total. The mechanism is that the capture's clock is whole seconds and the replay
  feeds each message at its stamp. See SESSION-JOURNAL.md 28, and the `tick-cadence` rule that
  says it only in the reports.
- **What only closes by playing**, and therefore does not advance on its own: a match with
  **three or more clients on bsPort** (which is what settles the extra second peer,
  `UNINTENTIONAL-DIVERGENCES.md` 2), a real **reconnection**, a forced **desync**, and one
  capture whose observer activates **between two steps**. All four are in `PENDING.md` 1.
  The question the fourth asked about the original — whether it serves a freshly activated
  observer in the same tick — is **closed from the binary**: it does not, and there is no arm
  that could (`SESSION-JOURNAL.md` §44.4).

## THE SETTLE IS THE OBSERVER STEP'S PERIOD, NOT THREE TICKS

The replay keeps reading after the last captured input for the observer step's own period,
which is `interval+2` ticks, **1,25 s** at the defaults. It used to be three simulation
periods (400 ms), which is **less than a third** of the step's period, and on the phases
where the step came round after that the replay closed the connection before the frame was
written and reported its own truncation as a divergence of the server — the 1-in-60
`TestSim_synthetic` flake and, with another signature, the 1-in-9
`TestReportSaysWhenEveryDivergenceHappened` one. The number comes from the server's own
`ObserverUpdateIntervalTicks`, so the wait and the thing waited for cannot drift apart, and
the report prints the arithmetic as a `settle window` deviation. **Enlarging a fixture does
not fix this**: the settle is always derived from the last whole second of the recording.
`SESSION-JOURNAL.md` §44.4, `internal/sim/replay.go` (`settleFor`).

## THE TWO TRANSPORTS, AND HOW THE HARNESS READS THEM

The proxy writes **two** captures with different shapes, and the harness has to read both:

| | `webSocketPort` | `bsPort` |
|--|--|--|
| one CSV row | one WebSocket frame | the whole RLink message, `[u16 BE length][type][body]` |
| `msg_type` column | `0x82` | the message type (`0x0A`, `0x13`, `0x09`, ...) |
| handshake | yes, and it is skipped | no |
| TLS | yes, while `-sslCert`/`-sslKey` are in force | **never**: the bs listener is built without the pair (`internal/server/server.go:195`; the one that receives it is the WebSocket one, `:199`) |

- **`msg_type` is decoration.** It is the proxy's assumption about what each row carries
  (`cmd/proxy/packets.go:33`), that is `0x82` on WebSocket and the message type on bsPort,
  where there is not a single row with `0x82` in the whole log: measured over
  `2026-09-30_201122_c4e7c284ff1c`, 2928 rows and `0x82 x0`. The reader has never read it,
  and `cmd/proxy/crosscheck_test.go` checks that the same capture with the column rewritten
  to `0x00` and to `0xFF` reads the same, with the same transport and the same messages.
- **The transport is decided by the bytes, and with the whole stream.** The first two bytes
  are not enough: an RLink message of 33.024..33.279 or 33.536..34.047 bytes starts `0x81` or
  `0x82`, byte for byte a WebSocket FIN+binary header. Each candidate is walked entirely in
  both directions and the one that reads without running into a byte that cannot start a
  frame wins; the HTTP upgrade, when it is there, overrides everything else.
- **A log whose rows all decode and out of which not one message comes out is
  rejected** (`internal/sim.loadLog`). Before, only a log whose rows could not be decoded
  was rejected, and that was the hole through which an unreadable capture passed as a clean
  capture and a green test on top of two empties.

The most expensive failure of this kind so far: TLS belongs to the **port**, not to the
run. The harness decided it once for the whole replay, so a bsPort capture dialled TLS
against a listener that does not speak TLS and ended up with `ours=0` against 24 and 2
messages from the original, with a clean report. See SESSION-JOURNAL.md 27.


The capture history (_1 to _30, 294 MB) was **deleted**; what was kept is in
`src_go/internal/server/testdata/observer_vectors.json` and in `CAPTURES.md`.

---

# MEASURING AGAINST THE ORIGINAL: `src_go/cmd/benchpeer`

`PERFORMANCE.md` is almost entirely a measurement of **our** code, and that has one blind
spot: it never says whether the original spent more or less. `benchpeer` closes that, from
the outside:

```
go build -o benchpeer ./cmd/benchpeer
benchpeer -bin ours.exe=ours,BattleServer.exe=original -window 15s -conns 8 -repeat 5
```

It launches both binaries, applies **identical rules** to both, and reports startup latency,
the idle footprint, the CPU each burns while holding a port with nobody on it, and the cost
of one held connection. **It knows a path, an argument list and a port, and not which
binary it is holding** — that is what makes the two columns comparable, and it is why it can
run against the original at all.

**Five rules for it, and they are why the numbers can be trusted:**

1. **It repeats, and reports the median with the range.** An early single run of this same
   comparison reported **51,7 MB** idle working set for ours and 11,9 MB on the next run. A
   single-run tool prints whichever it saw. Two figures in the table are only readable as a
   range, and the range is printed beside them. **Repeating is the right answer for one
   distribution and the wrong one for a bimodal figure**, which is why the WebSocket
   handshake is sampled `-upgrades N` times per repetition (default 5) and its samples are
   **printed**: a median of a bimodal set says which cluster won, not what happened.
2. **A figure it cannot measure is `n/a`, never 0.** `Thread32First` fails on this machine
   with the correct 48-byte `THREADENTRY32`, so thread count is absent rather than zero. This
   is the same failure as §2 above, in a different layer, and it is reported the same way.
3. **Every counter read carries its error out.** Two Win32 API facts had to be measured
   rather than looked up — the counters must come from `kernel32!K32GetProcessMemoryInfo`
   because the `psapi.dll` forwarder refuses every `cb`, and `cb` must be the *plain*
   structure's 72 bytes — and **neither failure is visible as a wrong number**: both return a
   zeroed structure. The figures are in `PERFORMANCE.md` §10 and the reasoning in the source.
4. **The row called "commit" is `PagefileUsage`, not `PrivateUsage`** — a third fact found by
   the second tool, and it is the one that renames the column. `PrivateUsage` is in the `_Ex`
   structure, the plain call refuses the 80-byte `cb`, so the figure is the commit charge.
5. **A sample the clock could not resolve is counted, not carried in as a zero.** This
   machine's monotonic clock reads **0 for 94 of 200** loopback TCP round trips, median 509 µs
   over the rest. A `0.00 ms` on the upgrade row is a clock, not a fast handshake, so the
   samples are stored and **printed in microseconds** — in milliseconds at two decimals every
   reading under 10 µs collapses into the same `0.00`. **Nothing below about 500 µs from this
   machine is a measurement.**

**It is a comparison of PROCESSES, not of algorithms**, and §10 says so in its own words: it
includes each runtime's idle footprint and GC headroom. **The 5,3× on commit charge is
32,55 MB of dirty executable pages plus an 11,7 MB Go runtime floor, and our own data is
2,35 MB** — measured, not asserted; see `PERFORMANCE.md` §10 and `SESSION-JOURNAL.md` §49.

---

# WHERE THE COMMIT CHARGE IS: `src_go/cmd/vadbreak`

`benchpeer` answers **how much**. It reports one number, and one number cannot say whether the
cost is the heap, the executable or the DLLs, and **those three have different remedies**.
`vadbreak` walks a process's address space with `VirtualQueryEx` and totals the **committed**
bytes by region type and protection, which is the attribution `benchpeer` does not carry.

```
go build -o vadbreak ./cmd/vadbreak
vadbreak -pid 12345        # a table; -json for the same figures, -pid 0 walks itself
```

It carries `benchpeer`'s rules, and the second one is the one it was written for: **a figure it
could not read is absent, never 0**, and off Windows it exits 2 with `UNAVAILABLE` rather than
printing zeroes. It prints the commit charge beside its own total **so the two instruments can
be reconciled**, and that reconciliation is what earns the numbers: it is what caught the two
bugs that had this repository contradicting itself, one of them reading `dwPageSize` at the
wrong offset of `SYSTEM_INFO`. The table is in `PERFORMANCE.md` §10.

---

# TRIMMING A CAPTURE: `src_go/cmd/trimsim`

Every capture is replayed in **real time** on purpose (the server emits `0x0A` per tick,
and a faster replay measures the harness and not the server). That is why a test costs what
the run cost, and why a 24-minute capture is a 25-minute test. The only way the suite fits
its budget is to **shorten the data**, never the clock.

A trim is safe if and only if the truth that remains is a **prefix** of what there was: the
truth of a capture is a RECORDING, so taking its tail off shows up in what the harness
compares. That is not asserted, it is checked:

```
captures/2026-09-30_010531_a553ef8fd63c/
        |
        |  trimsim -inspect <dir>        what it has, second by second (this is how you decide where to cut)
        v
        |  trimsim -trim <dir> -out <dst> -keep 4m10s -why "..."   writes the trimmed capture
        |  trimsim -verify <dir> <dst>   replays BOTH with the same rules and compares them
        v
internal/sim/testdata/<capture>/        only if -verify ACCEPTED
```

- `-verify` decides with `internal/sim.CheckTrim`: the same verdict, the original's
  client->server stream as a prefix, the server->client one as a prefix, the sub-message
  sequence and the observer's checksum entry sequence as prefixes, the starting point of
  its window equal, and **every connection compared before still compared after**. Faced
  with any problem it **does not write the proof and exits with an error code**: a trim
  that has not been tested is not imported.
- A trimmed capture carries its record in `metadata.json` with `format_version: 2` (what was
  saved, since when, why, and the evidence). Each test's report prints it in full, and
  **a trim without evidence comes out as NOT VERIFIED**, because "not verified" and
  "verified and correct" are different facts.
- `format_version: 2` **without** a record is **rejected**: a capture that claims to be
  trimmed without saying what was taken off it is not auditable.
- The tests for this are at two levels: `trim_test.go` without sockets (it fakes an empty
  stream, a moved window, a swallowed finding, a lost connection) and `trimreplay_test.go`
  with two real replays (one capture, its trim, the two compared).
- **What a trim cannot touch**: the handshake and the control responses
  (`0x19`, `0x1E`), the starting point of the observer's window, nor the order of the
  sub-messages or of the checksum entries. The last three are must-match and a suffix does
  not move them, which is exactly what `CheckTrim` checks.

### The budget, measured again (2026-10-03, with the 38)

> **The table below is from 2026-10-03. The most recent measurement, and the one to use, is
> in [`PERFORMANCE.md`](PERFORMANCE.md) 1** (with the 45, 2026-10-05, this tree: **~49 s /
> 113.6 MB** short and **775.6 s / 116.0 MB** full, `internal/sim` 770.8 s; the 44 had
> **52.3-55.1 s / 112.3 MB** and **778.5 s / 116.6 MB** under a machine at 54-71%, so the
> short loop's clock is **not** comparable between them and the full suite's is). **The suite
> is over the 600 s budget and the options are in [`PERFORMANCE.md`](PERFORMANCE.md) 9**,
> measured, and **none of them has been taken**. And the **why** of the caps, and the
> sampler's methodology, in `PERFORMANCE.md` 0 and 2. The table is left here for its context.

With the working set of the test binaries of **this repo** added up every 400 ms, and in
`src_go`. The filter matters and the 36.6 says so: on Windows `Get-Process` returns the name
**without** extension, so a filter on `*.test.exe` gives 0 MB; and a filter on the bare name
picks up other projects' `*.test`. The 37's sampler filters each binary by the command of
**its parent**, which has to carry the repo path:

| | 37 | 38 |
|--|--|--|
| `go test -short ./... -count=1` (short loop) | 45.3 s / 116.8 MB | **43.6 s / 110.8 MB** |
| `go test ./... -count=1 -timeout 25m` (full) | 772.4 s / 115.2 MB | **770.0 s / 115.6 MB** |
| `go test -race server+session+sim -timeout 25m` | 805.2 s / 303.1 MB | **790.1 s / 309.3 MB** |
| `internal/session` alone | 4.1 s / 50.1 MB (**18.8 s** `-race`) | **6.0 s** (**49-77 s** `-race`) |

The sampler's filter requires the **parent's** command to carry the repo path, and that only
happens if `go` is launched with `go -C <repo>`: launched from the working directory, the
parent is a `go test -short ./...` with no path and the sampler measures 0 MB without
warning.

**The short loop's time and the full suite's have not moved** (-1.7 s and
-2.4 s, within the noise), and neither has RAM. What goes up is `internal/session`, which is
where `tickcost_test.go` lives: its clock figures are measured in windows of 4096 ticks and
the race detector multiplies by thirteen the work of each tick. **It does not move the
critical path**, which is `internal/sim` (764.6 s of the full run's 770.0): `session`
and `server` run in parallel with it.

What is a real change underneath, and it is in §38.3: **the tick no longer allocates per
client.** It was 9.30 objects per peer and tick (310 in total at 32 peers) and it is now
0.00 per peer (6.05 in total). The caps that held it down are tightened accordingly
(12.0 -> 3.0 per peer, 400 -> 24 per tick), because a cap with twelve times of slack is
not a cap.

The capture `2026-09-29_101831_a553ef8fd63c` (24m44s) is in `captures/` and is **not**
imported: it is 1497 s and 259 MB measured, and it does not fit with the other four. Its
report says 0 must-match and 2 extra messages in 24 minutes against the original, so what
is lost by trimming it is tail of the same match. If you want to import it:
`trimsim -trim captures/2026-09-29_101831_a553ef8fd63c -out <dst> -keep
1m -why "..." -verify`, and only if `-verify` ACCEPTED. See SESSION-JOURNAL.md 26.

---

# bsPort: it is KNOWN that you CANNOT observe there

Fact confirmed by the user. The original stays at **a single peer** on bsPort
because there is no observer possible there, not because it counts differently.

Consequences, and they are the kind that save wasted time:

- The bsPort capture **is not a reference for observer behaviour**, because
  **it has no observer**: its two connections are two players', and the only things the
  client sends are `0x09`, `0x0F`, `0x13`, `0x14` and `0x15` (the totals, in
  `internal/sim/testdata_test.go`). For the observer the reference is WebSocket, where it is
  already known to match.
- The peer divergence that stays open on bsPort (31 extra `LobbyRelay`, extra acks,
  `fe 00 02 01 02 00 00` against `fe 00 01 01 00 00`) **is not about observation**, but
  about why we add a second peer on a transport where the original does not.
  **It is already localised**: the harness's `[pending-peers]` rule measures it on every
  replay. The exact condition is still unknown, see below.

## WHEN A SESSION PEER IS CREATED: THE ADDPEER, AND IT NEEDS A REQUESTJOIN FIRST

Already known from the binary and from the captures (`SESSION-JOURNAL.md` §29):

- **`sub_1400E5670` is the only function that writes a peer** into `session + 24 +
  8*peerID` (line 100), and it is reached by a single chain: the ADDPEER case of `sub_1400DAAF0`
  (`:298`) calls `sub_1400EAE60`, whose only caller is that same case, and it calls `sub_1400E5670` on its
  line 29. Before creating, it looks for the **global id** in the session's tree at
  `session+2080` (`:59-61`) and **returns without creating if it is not there** (`:75-85`).
- That tree is written by **one single** function, `sub_1400E5B00`, reached only from the
  REQUESTJOIN case of the same switch (`sub_1400EA680`, `:259`).
- **`sub_1400EB5F0` (the `0x15` handler) adds no peer on any of its paths.** That is where
  we add them, and that is where the divergence is.
- The client **does not send** either of the two as a top-level `0x0F`: it puts them
  inside a `0x13` and the original dispatches them recursively (`sub_1400DE050:567`).
  The first ones come **zlib compressed** (`flagBits 0x04`, `sub_1400DAAF0:75`): without
  inflating them the internal type is `0x78` and no peer message is visible at all.
- The pending list of the `0x0A` is the **participating** peers whose game data queue is
  empty when the tick starts, in **join order**, minus the ones that are past their deadline
  (which get closed) (`sub_1400E34F0:86-119`, `SESSION-JOURNAL.md` §33). The
  harness's `[pending-peers]` rule reads it from both sides and compares **sets**, not
  counts nor order. It costs zero seconds of game time. Note: **there is no cut** at the
  first non-participating peer; the jump in the binary is the bottom of the loop.

**What is still NOT known**, and why the server is not touched: in the bsPort
capture the client asks for the second peer with both halves (REQUESTJOIN + ADDPEER) and
the original does not create it, but the two messages go in **different connections** with a
whole-second clock, so **the order in which it processed them is not in the
capture**. Which of the two it rejected is unknown. What would settle it: a bsPort match
with **three or more real clients, no observer**, with the third client's `ADDPEER` arriving
**over the same connection** as its `REQUESTJOIN`. With two connections, "one peer per
connection" and "one peer in total" are indistinguishable.
