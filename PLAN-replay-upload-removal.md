# Plan — remove the replay uploader (`replayUpload`) from ageBattleServer

- **Status**: proposal, nothing executed yet.
- **Date**: 2026-11-03.
- **Scope**: the Go module at the root of this repository (`F:\ageBattleServer`) plus the
  documentation it carries in `docs/`.
- **One-line goal**: delete the code that pushes recorded game data to a replay host over
  HTTP, keep every command-line argument that belonged to the original
  `BattleServer.exe` (even if it now does nothing), and leave the in-process replay data
  callback and the observer frame store exactly as they are.

---

## 0. The three things called "replay" — only one is removed

A careless `grep -r replay` would take out two features that have nothing to do with the
uploader. This is the map:

| what | where | verdict |
|--|--|--|
| **The replay uploader**: dials `-replayHost:-replayPort`, zlib+base64 each `0x10` body and `POST`s it to `/game/replay/setReplayData?matchid=&sequenceid=` | `internal/replay/` (2 files), wired from `internal/server/server.go` and `internal/server/handlers.go` | **DELETE** |
| **The in-process replay data callback**: `Config.ReplayDataHandler` / `ReplayData`, delivered on the sender's goroutine | `internal/server/server.go:96-101`, `internal/server/handlers.go:458-460`, `battleserver.go:17-31,106-109` | **KEEP** — it is ours, it is the embedding feature, and it is the remaining way a host gets recorded data |
| **The observer frame store**: the paged in-memory store an observer is served from | `internal/session/replay.go`, `internal/session/session.go` | **KEEP** — protocol behaviour, unrelated to the uploader |

Two more "replay" names that must not be mistaken for the uploader:
`internal/sim/replay.go` (the capture *replayer* used by the test harness) and the
`uploadVia-lobby*.md` / `replays.md` documents (descriptions of the **original binary**).

---

## 1. Why remove it

- The original **does not upload**. Measured by the owner with a listener on the replay
  host, `-uploadVia=lobby`, and a match played to the end: **the TCP connection arrived and
  no request ever followed**. `battle-server.txt:18,23` agrees (uploader built, connect
  *deferred and never completed*, no request in the remaining 423 lines). This is already
  recorded in `docs/PENDING.md:126`, `docs/INTENTIONAL-DIVERGENCES.md` §6, and
  `docs/UNINTENTIONAL-DIVERGENCES.md` §1.
- So there is no byte stream of the original to be faithful to; the uploader is **our**
  feature, not a reproduction. Removing it cannot cost wire fidelity to a client, a peer or
  the replay host — the only wire artefact the original produced there was a TCP connect
  that never carried a byte.
- The owner has asked for it (this plan). The removal is therefore a product decision and
  has to be written down as one, because it is still observable **from the replay host**.

---

## 2. The arguments: what is original and stays, what is ours and goes

This is the part that must not be lost: **the seven replay-related flags are all the
original's**, so all seven stay registered and accepted. They become inert — parsed,
visible in `-help`, and read by nothing. None of them is one of our additions.

The machine-checked contract for this is already in the tree:
`internal/config/config_test.go:53-82` (`TestParseFlagsAcceptsOriginalFlagNames`) asserts
that **every** name read off the original's `_stricmp(v48, ...)` chain in `main` is
registered here, because flag parsing is strict and a missing name makes this server exit 2
where the real binary starts. Its list (lines 60-69) contains all seven.

### 2.1 Kept, on purpose, as no-ops

| argument | original's flag | evidence in this repo | after removal |
|--|--|--|--|
| `-uploadVia` | yes | `config_test.go:68`; `docs/replays.md:684`; `docs/uploadVia-lobby.md:5`; `SESSION-JOURNAL.md:8108` (`sub_1400D6790`, `v32 == 2`) | accepted, any value, ignored |
| `-replayHost` | yes | `config_test.go:67`; `docs/replays.md:685`; `replays.md:432` (`WorldwideReplayUpload + 0x08`) | accepted, ignored |
| `-replayPort` | yes | `config_test.go:67`; `docs/replays.md:686` | accepted, ignored |
| `-login` | yes | `config_test.go:62`; `docs/replays.md:704` (passed as the 4th ctor argument) | accepted, ignored |
| `-password` | yes | `config_test.go:66`; `docs/replays.md:705` | accepted, ignored |
| `-replayDBName` | yes | `config_test.go:67`; `docs/replays.md:706` | accepted, ignored |
| `-odbcDriver` | yes | `config_test.go:65`; the deleted `uploadVia-mysql/odbc` docs listed it | accepted, ignored |

Consequences to implement deliberately:

1. **They keep their names, types and defaults** in `internal/config/config.go` and their
   lines in `printUsage()`. The only edit to the help text is a note saying the upload is
   not implemented; renaming or dropping a name is forbidden by the test above.
2. **They bind to throwaway locals**, not to `server.Config` fields (see §3.2). The flags
   exist for launchers; the fields are ours and die with the feature.
3. **`-uploadVia`'s value validation goes.** Today `internal/config/config.go:116-124`
   exits 1 for `mysql`/`odbc` and for anything that is not `lobby`. That exit is **our**
   addition, not the original's: the original's enum has the three methods, and its
   uploader constructor receives the DB credentials as arguments — `NULL` only "for lobby"
   (`docs/replays.md:690-707`) — which is what `-login`, `-password`, `-replayDBName` and
   `-odbcDriver` are for. Since none of the three is implemented now, the honest
   behaviour is: accept **any** value, ignore it, and emit **one** log line when any of the
   seven replay flags was on the command line. Keeping `exit(1)` would mean a launcher that
   passes the original's own `-uploadVia mysql` is refused by a flag that does nothing —
   the opposite of "keep the argument". **Level: `Info`, not `Warning`**: a launcher that
   always passes the flag (the original's docs show exactly that) would otherwise warn on
   every single start; the line is a notice, not a fault.
4. A test that used to pin the exits (`cmd/battleserver/main_test.go:98-110`) is replaced by
   one that pins the **acceptance** and the **inertness** (§5).

### 2.2 Ours, and removed

| what | where | why it goes |
|--|--|--|
| `internal/replay.UploadManager` (+ its tests) | `internal/replay/uploader.go`, `uploader_test.go` | the feature itself |
| `server.Config.UploadVia/ReplayHost/ReplayPort/Login/Password/ReplayDBName/OdbcDriver` | `internal/server/server.go:32-38` | not arguments: fields that only existed to feed the uploader (the last four are written by `ParseFlags` and read by nobody else — verified) |
| `Server.replayUploader` field, `New` branch, `Start`/`Stop` calls, `ReplayUploader()` accessor | `server.go:169,253-256,335-337,368-370,571-573` | wiring |
| the `handleProxyData` upload branch | `handlers.go:461-470` | wiring |
| `battleserver.Config.UploadVia/ReplayHost/ReplayPort`, `Server.ReplayUploadEnabled()` | `battleserver.go:61-68,178-180,205-207,237-241` | public API of ours; see §6 for the breaking-change decision |
| dependencies `github.com/valyala/fasthttp` and `github.com/klauspost/compress` | `go.mod:6,8` | imported **only** by the uploader (verified: no other Go file in the module imports either) |

---

## 3. Code changes, file by file

### 3.1 Delete

- `internal/replay/uploader.go` (352 lines)
- `internal/replay/uploader_test.go` (~500 lines)
- the directory itself: it holds nothing else and nothing else may import it afterwards.

### 3.2 `internal/config/config.go`

| lines today | change |
|--|--|
| 39-45 | keep all seven `flag.*Var` registrations, identical names/defaults/descriptions; point them at a local `legacyReplayArgs` struct (or plain locals) instead of `cfg` fields. Add a comment block: these are the original's arguments, kept so an original launcher starts; nothing reads them. |
| 116-124 | delete the `switch cfg.UploadVia` with both `os.Exit` paths. |
| 126-136 | extend the existing `flag.Visit` pass (or add one) to note whether any of the seven was set, and emit one `logger.Info("Main","parseFlags", ...)` saying the replay upload is not implemented and the flags are accepted and ignored. No line when none was given. |
| 173-180 | keep the `Replay Options:` block and every line; add one line under the header: the upload is not implemented and these flags are accepted and ignored. Suggested wording keeps the original names untouched, e.g. `-uploadVia string  Replay upload method (accepted; upload not implemented)`. |

### 3.3 `internal/server/server.go`

- line 14: drop the `internal/replay` import.
- lines 32-38: delete the seven fields.
- line 169: delete `replayUploader *replay.UploadManager`.
- lines 253-256: delete the construction branch and its log line.
- lines 335-337 (`Start`) and 368-370 (`Stop`): delete the two blocks.
- lines 571-573: delete `ReplayUploader()`.

### 3.4 `internal/server/handlers.go`

`handleProxyData` (450-470): keep the single `FindSessionByPeer` lookup, the `sessionID`
computation and the `ReplayDataHandler` call; delete the `if s.replayUploader != nil` block.
Update the comment that says "One lookup for both the recording hook and the upload".

### 3.5 `battleserver.go` (public API)

- lines 3-4: the package doc says "instead of (or in addition to) the cloud replay upload" →
  reword to the callback only.
- lines 61-68: delete `UploadVia`, `ReplayHost`, `ReplayPort` and their comments.
- lines 106-109: keep `ReplayDataHandler`, drop the sentence about `UploadVia`/`ReplayHost`
  being ignored.
- lines 178-180 and 202-212: delete the mapping of the three fields and the
  "callback takes over" block that only existed to blank them.
- lines 237-241: delete `ReplayUploadEnabled()`.

### 3.6 `internal/sim` (the harness)

| file:lines | change |
|--|--|
| `internal/sim/replay.go:374-378` | delete the `ReplayHost/ReplayPort` rewrite and its deviation entry: there is nothing to turn off any more. Update the comment at `:329` that lists the replay host among "what the capture needs". |
| `internal/sim/sim.go:1155,1170,1334-1337` | delete the `uploadVia` capture and the `checkUsageExits` branch that rejected non-`lobby` values (the server no longer exits). Update the comments at `:1121` and `:1132`. |
| `internal/sim/sim_test.go:866-880` | delete the `uploadVia` case from the "came never ran" table; keep help / simulation-zero / missing-value. |
| `internal/sim/prepareConfig` | nothing else: no committed capture passes a replay flag (checked all `testdata/*/metadata.json`), so no fixture changes. |

### 3.7 Tests: delete or rewrite

| file | what |
|--|--|
| `internal/replay/uploader_test.go` | deleted with the package |
| `internal/server/handlers_test.go` | import of `internal/replay` (line 17); `newTestUploadManager` (350-353); `TestHandleProxyDataWithUploader` (932-949); `TestReplayUploaderAccessor` (1810-1819). **Keep** `TestHandleProxyDataReplayCallback` and `...NoSession` (355-398) — they cover the surviving callback |
| `internal/server/integration_test.go` | `TestReplayUploadUsesSessionID` (479-533), `TestReplayUploadRequestShapeAndPayload` (535-662), `TestReplayUploadSkippedWithoutSession` (664-707). Then drop the imports that only they used: `compress/zlib`, `encoding/base64`, `net/http/httptest` (all three `httptest` uses are in those tests), and `net/http`/`strconv`/`io` if goimports finds them unused |
| `internal/server/server_test.go` | fields `UploadVia/ReplayHost/ReplayPort` (43-45) and the `replayUploader == nil` assertion (75-77) |
| `battleserver_test.go` | `TestReplayUploadWithCallbackDisabled` (77-91) and `TestReplayUploadWithoutCallback` (93-106). **Keep** `TestReplayDataHandlerEndToEnd` (108-166) |
| `internal/config/config_test.go` | keep the args in `TestParseFlagsValues` (147-153) — they now prove the flags still parse — and delete the field assertions (209-226) and the `UploadVia == ""` default check (128-130). **Do not touch** `TestParseFlagsAcceptsOriginalFlagNames` (53-82): it is the guard that keeps all seven names registered |
| `cmd/battleserver/main_test.go` | helper cases `uploadvia-mysql` / `uploadvia-invalid` (48-51) and their tests (98-110) |

### 3.8 New tests (the ones that prove the contract)

1. **`cmd/battleserver`: the original replay arguments are accepted and inert.**
   A helper-process test (the file already has that pattern) that:
   - binds a listener on `127.0.0.1:0` in the parent and passes its port to the child;
   - the child parses `-uploadVia lobby -replayHost 127.0.0.1 -replayPort <port> -login u
     -password p -replayDBName db -odbcDriver d -bsPort 0 -outOfBandPort 0 -webSocketPort 0
     -relaybroadcastPort 0 -logLevel none`, builds the server with `server.New`, runs
     `Start`/`Stop`, and exits 0;
   - the parent asserts the child exited 0 **and** that `Accept` with a short deadline times
     out: no connection to the replay host, ever.
   This is the only test that can prove "does nothing" from the outside.
2. **`internal/config`: value acceptance.** Parsing `-uploadVia mysql`, `odbc`, `lobby` and
   a bogus value returns normally (no `os.Exit`), and the rest of the config is unchanged.
3. **`internal/config`: help text.** `printUsage` still prints `-uploadVia`, `-replayHost`,
   `-replayPort`, `-login`, `-password`, `-replayDBName`, `-odbcDriver` (a substring check is
   enough; `go test` cannot capture `fmt.Println` easily, so this may instead stay as a
   manual check in §7 — decide at implementation time, do not contort the test).

---

## 4. Documentation changes

The repository's docs are the research side and most of them describe the **original**. They
are not deleted just because we stop implementing the feature; they get a banner saying so.
`docs/SESSION-JOURNAL.md` is history and is **not edited** (it gets a new section only if the
owner wants the round recorded there).

| file | change |
|--|--|
| `docs/replays.md` | banner after the H1: this describes the original's replay pipeline; **this implementation does not upload**. Fix the "Error Handling" paragraph (heading at 84, retry sentence at 86-88) that says the Go implementation retries once. Keep everything else: it is the only record of the request builder. |
| `docs/uploadVia-lobby.md` | banner (same wording) at the top; keep the HTTP contract; label the `BattleServer.exe` launch examples as the original's. |
| `docs/uploadVia-lobby-connection.md` | banner; keep the "connect deferred" diagnosis (it is the evidence for §1). |
| `docs/uploadVia-lobby-state.md` | banner; its "Option A / Option B: modify the ..." section is about the original's code — mark it historical. |
| `docs/README.md` | 43-45 (the three table rows), 74-78 (the "mysql/odbc are rejected" paragraph — no longer true), 97 (the "replay upload" row), 104-106, 148 (`-uploadVia` default row: "only value accepted" → accepted and ignored), 156-157 (the exits). |
| `docs/PENDING.md` | 126: keep the measurement, add that the feature has been removed from this implementation and that `TestReplayUploadRequestShapeAndPayload` no longer exists (point here). |
| `docs/INTENTIONAL-DIVERGENCES.md` | retire §4.2 ("the uploader is not fed from `0x13`"), §5 ("the upload without a session is discarded") and the §6 bullet "the byte range of the replay upload" — none of them can happen any more. Add **one** entry: *we do not open the replay socket at all; the original opened it and never completed it nor sent a request*. Reason: nothing to lose on the wire, the connect was the only artefact. Evidence: the measurement above, `battle-server.txt:18,23`, `SESSION-JOURNAL.md` §52. Follow the file's §7 "retired" pattern for the three removals. |
| `docs/UNINTENTIONAL-DIVERGENCES.md` | §1 (70-115) stays closed as history; add one line saying the uploader has since been removed, so the question no longer applies. |
| `docs/bsPort.md` | 392: the `-replayHost:replayPort / TCP / Replay upload` row → no outbound connection any more. |
| `docs/LOBBY-PROTOCOL.md` | 11-21: the "Replay Upload → HTTP POST / MySQL / ODBC" list describes the original; add "(not implemented in this server; the flags are accepted and ignored)". |
| `docs/WEBSOCKET-BSPORT-COMPARISON.md` | 83 and 346 mention the uploader as present in both; optional one-line note that this describes the original. |
| `USE_CASES.md` | 17: delete "Enable replay uploading." |
| `README.md` (root) | 19 ("Optional replay delivery hooks" → replay data callback) and 44-45 ("replay integration" → the callback). |
| `docs/AGENTS.md` | **leave**: it maps the documents, and the documents stay. |
| `docs/revision.MD`, `docs/PERFORMANCE.md` | leave: history, and the `session+2140` row describes the original's field. |

---

## 5. Order of work and commits

Rules from the repository guidance: signed commits, nothing half-done in the tree, build +
vet + tests green at every commit, commit before moving/renaming.

1. **Commit 1 — code** (`refactor: remove the replay uploader`): §3.1-§3.7 and the new tests
   of §3.8 in one commit, because deleting the package alone does not compile. Green:
   `go build ./...`, `go vet ./...`, `go test -short ./...`.
2. **Commit 2 — dependencies** (`build: drop the uploader-only dependencies`):
   `go mod tidy`; `fasthttp` leaves `go.mod`, `klauspost/compress` leaves or becomes
   `// indirect` if `gws` needs it transitively; `go.sum` follows. Verify with
   `go mod tidy -diff` (empty) and a full build.
3. **Commit 3 — docs** (`docs: record the removal of the replay upload`): §4.
4. **Optional, if the owner wants it**: a new `docs/SESSION-JOURNAL.md` section recording
   the round and the measurement, and a version bump (`internal/server/server.go:18`, today
   `2.58.0`) if the public API change is released as a breaking version.

Each commit message should name the argument contract, e.g. "the original's
`-uploadVia/-replayHost/-replayPort/-login/-password/-replayDBName/-odbcDriver` stay
registered and inert".

---

## 6. The two decisions to confirm before starting

1. **Public API (`battleserver.go`)**: removing `Config.UploadVia/ReplayHost/ReplayPort` and
   `Server.ReplayUploadEnabled()` is a **compile-time break** for an embedder that sets them
   (ageLANServer is the obvious candidate). Recommendation: remove them, because they are
   ours and a knob that does nothing is worse than a compile error, and check the consumer in
   the same round. Fallback if the consumer cannot be updated now: keep the three fields and
   the accessor as deprecated no-ops (`ReplayUploadEnabled()` always false, fields ignored),
   and delete them in the next major version.
2. **`-uploadVia` values**: recommendation is accept-and-ignore every value, with one notice
   (§2.1 point 3), which is the only reading consistent with "keep the original's
   arguments". The alternative — keeping today's `exit(1)` for `mysql`/`odbc` — is smaller
   but leaves the launcher-facing behaviour saying "only lobby is supported" about a feature
   that supports nothing.

Everything else in this plan follows from those two answers.

---

## 7. Verification (definition of done)

Commands, from the repository root, in this order (the full suite replays captures and takes
minutes; the short loop is the working loop):

```powershell
$env:CGO_ENABLED = "0"
go build ./...
go vet ./...
go test -short ./... -count=1

$env:CGO_ENABLED = "0"
go test ./... -count=1          # full suite, ~13 min here; run before the last commit

$env:CGO_ENABLED = "1"
go test -race ./internal/server/ ./internal/session/ -count=1
```

Then the checks that are specific to this removal:

1. **No reference survives in Go** (docs are allowed to keep theirs):
   ```powershell
   git grep -nE "internal/replay|replayUploader|ReplayUploader|ReplayUploadEnabled|NewReplayUploadManager|setReplayData|fasthttp|klauspost/compress" -- "*.go" go.mod go.sum
   ```
   → no hits.
2. **The arguments still exist**:
   `go run ./cmd/battleserver -help` shows the seven names, and
   `go test ./internal/config -run TestParseFlagsAcceptsOriginalFlagNames` passes unchanged.
3. **The original launcher starts**: run the server with the full original replay flag set
   (any values) and confirm it starts, logs the one notice, serves normally and exits 0 on
   Ctrl-C.
4. **It really does not dial**: with `nc`/a PowerShell listener bound on `-replayPort`, start
   the server with `-uploadVia lobby -replayHost 127.0.0.1 -replayPort <that port>` and a
   match on `-bsPort`; the listener must see **no connection** (the automated version is
   §3.8.1).
5. **The harness still runs**: `go test ./internal/sim -short -count=1` (its
   `ConfigFromArgs` / `prepareConfig` paths changed) and one real capture test.
6. **Docs links**: no `docs/` page links to a file that no longer exists; the three
   `uploadVia-lobby*` files and `replays.md` still exist, so the links stay valid, and the
   `docs/README.md` table matches the tree.
7. **`go mod tidy -diff`** prints nothing after commit 2.
8. **Language**: all new prose is English, as the repository's documentation rule requires.
   The parent tree's `scripts/check_language.py --strict` is the tool that proves it, but
   `scripts/` is empty in this module, so run it where it exists or check by hand.

---

## 8. Risks and rollback

| risk | mitigation |
|--|--|
| an embedder sets the removed `battleserver.Config` fields | decision 1 above: check the consumer first, or deprecate instead of removing |
| a launcher depended on `exit(1)` for `-uploadVia mysql/odbc` to detect a misconfiguration | the notice at startup, and the docs entry; the original never exited for those values either |
| accidental deletion of the frame store or the callback (both are called "replay") | §0 is the map; `internal/session/replay.go` and `ReplayDataHandler` are explicitly out of scope, and their tests are listed as "keep" |
| docs drift: pages claiming the server uploads | §4 lists every page that says so; the `docs/README.md` table is the index to re-read at the end |
| the divergence register becomes false (it listed upload behaviour) | the new `INTENTIONAL-DIVERGENCES.md` entry and the three retirements are part of commit 3, not an afterthought |

Rollback is a normal `git revert` of commit 1 (code) plus commit 2 (deps); the docs commit
stands on its own and can stay if the code comes back.

---

## 9. Inventory appendix — every reference the change touches

Grep of the tree as it is today (`replayUpload|ReplayUpload|uploader|uploadVia`), grouped so
the implementer can tick them off:

- **Code**: `internal/replay/{uploader.go,uploader_test.go}`, `internal/server/{server.go,handlers.go}`,
  `internal/config/config.go`, `battleserver.go`, `internal/sim/{replay.go,sim.go}`.
- **Tests**: `battleserver_test.go`, `internal/server/{server_test.go,handlers_test.go,integration_test.go}`,
  `internal/config/config_test.go`, `cmd/battleserver/main_test.go`, `internal/sim/sim_test.go`.
- **Build**: `go.mod`, `go.sum`.
- **Docs**: `docs/replays.md`, `docs/uploadVia-lobby.md`,
  `docs/uploadVia-lobby-connection.md`, `docs/uploadVia-lobby-state.md`, `docs/README.md`,
  `docs/PENDING.md`, `docs/INTENTIONAL-DIVERGENCES.md`, `docs/UNINTENTIONAL-DIVERGENCES.md`,
  `docs/bsPort.md`, `docs/LOBBY-PROTOCOL.md`, `docs/WEBSOCKET-BSPORT-COMPARISON.md`,
  `USE_CASES.md`, `README.md`.
- **Not touched**: `docs/SESSION-JOURNAL.md`, `docs/revision.MD`, `docs/PERFORMANCE.md`,
  `docs/AGENTS.md`, `internal/session/**`, `internal/sim/testdata/**`, `internal/loadgen/**`
  (its "uploader" is a player role in the load profile), `cmd/**` other than
  `cmd/battleserver/main_test.go`.
