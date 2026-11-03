# Pending: what is missing, and who can resolve it

> **What this file covers.** Everything still open, split by **who can close
> it**. It is the work list: what needs a real match is done by the
> user, what is read out of the binary is done by anyone, and what is waiting on a
> decision is said here.
>
> **Status.** Current. The open bugs and the `UNKNOWN`s are also in
> [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md), with the
> evidence; here is the **count** of what is missing, without repeating the argument.
>
> **Priority.** By what it costs to resolve, not by age. The first section is the
> one that only closes by playing.

---

## 1. What only closes with a real match

These seven need the game and several clients. **They cannot be forced from here**, and
they cannot be deduced either: they are precisely the gaps the harness still cannot
ask. In order of value.

### 1.1 Row 1 of the matrix: AoE2DE, full match, **original at the front**

- **Why**: it is the reference. Of the five committed captures, **four** have our
  server at the front and only one (**`2026-09-28_205830`**) has the original, and none has
  it with an observer. The comparison with the original at the front is the one normal play
  exercises.
- **What it would resolve**: the full comparison, with the original as truth.
- **Status**: the matrix in `CAPTURES.md` has it unchecked. With the short loop it no
  longer costs 700 s per change, so the new capture goes in on its own.

### 1.2 Which of the two messages the original rejected when it did not create the second peer (bsPort)

- **Why**: the original stays at a single peer and we are at two, and **it is not known which
  of the two messages** (REQUESTJOIN or ADDPEER) was the one rejected. The two go in
  different connections with a whole-second clock, so the order in which they were processed
  is not in the capture.
- **What is needed**: a LAN match with **three or more real clients on bsPort**, with the
  game session started (so that the `0x0A`s exist) and **no observer**. With two
  connections, "one peer per connection" and "one peer in total" are indistinguishable; with
  three, if the original stays at 1 or goes up to 2, the question answers itself. If
  additionally the third client's `ADDPEER` arrives **over the same connection** as its
  `REQUESTJOIN`, the order is observable and it tells which of the two was rejected.
- **Full detail**: `UNINTENTIONAL-DIVERGENCES.md` point 2.

### 1.3 The destination bytes of each `LobbyRelay`, not just the count

- **Why**: the distribution of the 31 extra `LobbyRelay` needs a non-null slot in
  `session + 24 + 8*toPeerID` (`sub_1400E6160`), and with the count alone you do not know
  **which peer each one points at**.
- **What is needed**: the same as 1.2; it is the second step of the same match.

### 1.4 `0x0F`: if it creates a peer, seeing it brings the creation path

- **Why**: the journal has been asking for this since before §29. The client sends
  neither REQUESTJOIN nor ADDPEER as a top-level `0x0F`: it puts them inside a `0x13` and
  the original dispatches them recursively (`sub_1400DE050:567`), zlib compressed
  (flagBits `0x04`, `sub_1400DAAF0:75`). The 13 peer messages **can already be read** from
  the captures; what is missing is a match where one of them has an effect.

### 1.5 A real reconnection

- **Why**: **none of the five committed captures contains a reconnected peer**. Everything
  about the reconnected case is modelled by reading the binary and **not checked** against
  the original's bytes: the `secret`, that the replaced connection is closed, that only state 2
  resends frames, and that the returned `LobbyConnect`'s `dataReceivedCount` is returned.
- **How it is provoked**: kill and reopen the match halfway. Look at the `secret` and that
  it keeps the slot (`6884fa3`).
- **Matrix row**: 6.
- **Full detail**: `UNINTENTIONAL-DIVERGENCES.md` point 7.

### 1.6 A forced desync

- **Why**: reconciliation is modelled and has tests, but a real desync going through it has
  never been seen. The `0xFB` notice goes out empty in the original
  (`sub_1400DAAF0`), which is already written down, but reconciliation really consuming is not.
- **How it is provoked**: alter one byte of the stream. Look at the `0xFB` notice and that
  reconciliation consumes (`14c1eb2`).
- **Matrix row**: 7.

### 1.7 A capture whose observer activates BETWEEN two steps

- **What is left of the old 1.7**: not the question about the original — that is **closed**.
  The original does **not** serve a freshly activated observer in the same tick, and there is
  no arm in the binary that could: `sub_1400E43E0` builds exactly one message, the `0x1E`,
  and never calls the two functions that fetch frames out of the store. See
  [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 6 and
  [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §44.4.
- **What is left**: a question only a capture can answer — **does a real observer's first
  frame land on the tick the step comes round, as the arithmetic says?** The bound is now
  measured rather than argued (`internal/session`, `TestTheWorstPhaseCostsTheWholePeriod`:
  up to `interval+1` ticks, 1,125 ms at the defaults), so this row is about **seeing it once
  on the wire**, not about the number.
- **What is needed**: a capture with an observer whose `0x1E` (ActivateObserver) arrives in
  the middle of the stream, with enough match after it to hold several step periods. The
  activation has to fall **between two steps**, not aligned with one.
- **Why it is still a row**: a real run would also say whether the real client runs with
  the 8-tick interval the default gives, which no capture can say, because the harness
  reproduces OUR configuration (`-observerUpdateIntervalMS` was not in any of the five).
- **Full detail**: [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 6.
---

## 2. What closes by reading the code

None needs a capture. They are code reading.

| # | what | where | why it is still open |
|---|---|---|---|
| 2.1 | `session+2140`: which metric the "already published" latch is | `sub_1400E7600` / `:E7610`, only pair of calls in `sub_14010F4F0` | **Not a divergence**: it does not travel on the wire, so no client, peer or replay host can tell anything apart, and it left the register on 2026-10-05 as `UNINTENTIONAL-DIVERGENCES.md` **10.1**, a subsection that no longer exists (the note at its `## 10.` is what records the move). What is left is the **name** of a field nobody reads for behaviour. The two config parsers that read it have no call in the decompiled set. What is known of it is in [`PERFORMANCE.md`](PERFORMANCE.md) 5 |
| 2.2 | `conn+88`: the **reconnect** branch never writes it | `sub_1400EB5F0:133-463`, which swaps the peer onto the new connection at `:137-139` | **the writer question is closed** on 2026-10-05: it is `LobbyConnect` at `:469` and `:490`, **both on a first connect** (`:490` is the `v13 == 0` case), there is no third writer in the 7,450 files of `src/`, and the premise it was filed under was a misreading of which `if` the `else if` binds to. What the field is and what it gates is in [`PERFORMANCE.md`](PERFORMANCE.md) 5. **What is left is this**: the reconnect branch never writes it, so a reconnect can only work on a connection that already carries the byte, and no capture has a reconnection. Same blocker as 1.5 |
| 2.3 | `session+2176`, `+2224`, `+2232`, `+2240` | counters read by `sub_1400E9E90` (munin) and `sub_1400EBE60` (dump); written by `E9E90:153`, `E2590:361`, `EB5F0:136` and `E3260:32-36` | **Not divergences**: their readers are exactly the munin and the state dump and neither travels on the wire. They left the divergence file on 2026-10-05. What each one is and what writes it is in [`PERFORMANCE.md`](PERFORMANCE.md) 5; what is left is naming `+2240`, the widest span anyone asked for |
| 2.4 | The consumer of `maxreplay` | `sub_1400D3570`, `sub_1400D36E0` (both read `result`, `maxreplay` and `maxClientDosAttackCount` from the same struct) | reachable only through the vtable or from code that is not decompiled. **The citation that was retired**: `sub_1400D8E10:1145-1146` is not the decrement of `maxClientDosAttackCount`, it is that of a counter in the TLS's filename table |
| 2.5 | ~~Whether the nested code of `conn+88` is badly decompiled or there is a writer that does not appear~~ | `sub_1400EB5F0:131` and around | **CLOSED 2026-10-05, from the binary**: neither. The decompiler's braces are correct — `:464` closes `if (v15 != 0)`, so `:465` is its **sibling** and `:485` closes `if (v13 != 0)`, so `:486`'s `else if` is that one's `else`. Both writers are therefore **outside** `if (v15 != 0)`, and `:490` is the no-session case, i.e. a first connect. Exhaustive search of `src/`: no other byte-sized store to `+88` writes 1, so zero-initialisation cannot produce the value either. See [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §45.2 |

The detail of each one, with what is needed to close it, is in
`UNINTENTIONAL-DIVERGENCES.md` §10 for 2.2, and in [`PERFORMANCE.md`](PERFORMANCE.md) 5 for the
three that left.

---

## 3. What is waiting on a decision

| what | the decision | who |
|---|---|---|
| ~~**The byte range uploaded to the replay host**~~ | **CLOSED 2026-10-07 as a decision, not as an answer: the original does not upload.** Measured by the user with a listener, `-uploadVia=lobby`, and a match played to the end: the TCP connection arrived and no request ever followed. `battle-server.txt:18,23` agrees with the only recorded run. Moved to `INTENTIONAL-DIVERGENCES.md` 6 with the reason, and **what replaced the chase is a test**: `TestReplayUploadRequestShapeAndPayload` pinned the request against what the binary establishes, and labelled the one field that stays our reading. **The uploader has since been removed from this implementation entirely** (2026-11-03), so that test no longer exists and the question can no longer arise here; see `INTENTIONAL-DIVERGENCES.md` 6 and 7. `SESSION-JOURNAL.md` §52 | **closed, and not to be reopened** |
| ~~**Enlarging the synthetic fixture**~~ | **REFUTED, and removed on 2026-10-06: it was not a fix.** It was filed as "more seconds of recording after the activation, or a one-tick interval, removes the `TestSim_synthetic` flake", and [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) §11.1 says in its own words that **enlarging the fixture would NOT have fixed it**: the settle is always derived from the last whole second of the recording, so a fixture of any length keeps landing on the same 1-second tail and the same phase coin-flip. The flake was the harness's **own settle** (400 ms against a 1,125 ms observer step), and the settle's floor is now derived from the same `ObserverUpdateIntervalTicks` the session is configured with. It survived this long because its "who" column said *next session*. | **closed, and not to be reopened** |
| **Trimming the 24m44s capture** | `2026-09-29_101831_a553ef8fd63c` is 1497 s and 259 MB measured, and it does not fit with the other four. Its report says 0 must-match and 2 extra messages in 24 minutes against the original, so what would be lost is tail of the same match. If you want to import it: `trimsim -trim <dir> -out <dst> -keep 1m -why "..." -verify`, and **only if `-verify` accepts** | user: decide whether it is worth it |
| **The store's state at start-up** | whether a session should start with the store already cycled, which is what `testdata/synthetic-rollover` found | next session |
| ~~**`-maxPagesToStorePerSession` with no cap**~~ | **CLOSED 2026-10-07: no cap, the original's behaviour.** `src/main_1400d1310.cpp:617` computes `f = flag / 8` with no upper bound, and ours does the same (`internal/session/session.go:1045`), so **the ceiling is the operator's on both sides**. Adding a cap would have been a divergence and not a guard rail. See `SESSION-JOURNAL.md` §51 | **closed, and not to be reopened** |

---

## 4. The capture matrix, and where it is

`CAPTURES.md` has the live matrix and the procedure. The status:

| # | what | status |
|---|---|---|
| 1 | AoE2DE, full match, **original at the front** | **[ ]** pending (see 1.1) |
| 2 | AoE2DE, **observer**, original vs ours | **[ ]** |
| 3 | AoE3DE, full match | **[ ]** |
| 4 | AoE2DE, **ours at the front**, the failure that stays open (buildings of `_27`) | **[ ]** |
| 5 | observation | **[x]** `2026-09-29_012528`, ours at the front. Uncovered the frame store |
| 6 | reconnection | **[ ]** (see 1.5) |
| 7 | forced desync | **[ ]** (see 1.6) |
| 8 | `0x14` echo | **[ ]** 2 of 26 in `_26`, never isolated |
| 9 | match from the lobby | **[ ]** if the client ever sends `RequestJoin`. §16.4 reclassified it: **we now know it does not**, the session is created in the `LobbyConnect` |

**Eight of the nine rows are still pending** (5 is done). Row 2 already has its opposite done
— the captures `2026-09-30_010531` and `2026-09-30_183120` have an observer, with
**ours** at the front— so what is missing is the same test with the **original** at the front.

And the nine of the matrix are in `CAPTURES.md`, not here, so that the matrix and its
procedure do not get separated.

---

## 5. What is NO LONGER pending, so that nobody looks for it again

Decisions that were taken and closed. Each one with where it is written down.

| what was taken as open | where it was closed |
|---|---|
| Whether the BattleServer is deterministic, and the observer's zoom | `CLOSED-QUESTIONS.md` |
| The condition of the `0x19` StartObserve, and that the acceptance byte is **0** and not 3 | `SESSION-JOURNAL.md §22`, `§24`, `§28` |
| What the frame store keeps, and that it is a number of **BLOCKS** | `SESSION-JOURNAL.md §24` |
| RLink framing against WebSocket, and that the first two bytes are not enough | `SESSION-JOURNAL.md §27` |
| That the TLS is **the port's** and not the run's | `SESSION-JOURNAL.md §27`, `§28` |
| That a top-level `0x0F` is not what creates the peer: it is the ADDPEER, and a REQUESTJOIN is needed first | `SESSION-JOURNAL.md §29` |
| That on bsPort **you cannot observe** | user's fact, `SESSION-JOURNAL.md §27` |
| The eleven `UNKNOWN` fields of the audit: nine have a verdict, two remain | `SESSION-JOURNAL.md §32.8` |
| That the `0x0A` pending list **has no cut** | `SESSION-JOURNAL.md §33` (§32 read it backwards) |
| That closing a kicked observer is a **self-deadlock** if it is closed from inside the lock | `SESSION-JOURNAL.md §36.1` |
| That the high byte of `E2590:483` is **dead** in the original | `SESSION-JOURNAL.md §36.3` |
| The 9.30 objects per peer and tick: they are **0.00** | `SESSION-JOURNAL.md §38.3` |
| That there are no goroutine leaks | `SESSION-JOURNAL.md §35` |
| That the `TestSim_synthetic` flake **was not** about clock | `SESSION-JOURNAL.md §38.4` |
| That "we tick differently" is 4 acks from the replay, not the server | `SESSION-JOURNAL.md §28` |
| That the number of replay frames is **not** must-match | `AGENTS.md`, "The two levels of verdict" |
| That the session is created in the `LobbyConnect` and the game **never** sends `RequestJoin` | `SESSION-JOURNAL.md §4` (finding `_7`) |
| That the beacon does not advertise the WebSocket port | `INTENTIONAL-DIVERGENCES.md` 1.1 |
| That the original has **no** "host only" relay path | `SESSION-JOURNAL.md §16.4` |
| That the original's dispatcher **has no case for a top-level `0x01`**, and that `sub_1400EA680`'s setup is only reachable from a peer message | `INTENTIONAL-DIVERGENCES.md` 1.2 and 7, and **§44.2 of the journal**: the handler is gone, `0x01` lands in the `default`, and `TestTopLevelRequestJoinIsAnUnknownMessageType` keeps it there |
| That we send a **WebSocket ping** where the original answers with a **pong** | **It was never either.** `0x65` and `0x6B` are the `e` of `Sec-WebSocket-Accept` and the `k` of the accept key, the first bytes of the second TCP record of the `101`; no capture carries a `0x89` or a `0x8A` on either side. The real difference it hid — a missing `Server` header — is fixed and compared. `UNINTENTIONAL-DIVERGENCES.md` 5 and journal **§44.3** |
| Whether the original serves a **freshly activated observer in the same tick** | **It does not**, and there is no arm that could: `sub_1400E43E0` builds only the `0x1E` and never calls the frame fetch. The cost is `interval+1` ticks, measured. `UNINTENTIONAL-DIVERGENCES.md` 6 and journal **§44.4** |
| How two observers that share a `peerIdentifier` are **paired** | **First match wins**, and it is one line of the binary: `sub_1400E43E0:43` and `sub_1400E46C0:25`. `internal/session/observeridentity_test.go` |
| The `TestSim_synthetic` flake (1 in 60) and the `TestReportSaysWhenEveryDivergenceHappened` flake (1 in 8 to 10) | **Both closed**, and neither was a divergence: one was the harness's settle being shorter than the observer step's period, the other an assertion that needed a number of 125 ms ticks to fit in one wall-clock second. Journal **§44.4**; `internal/sim/replay.go` (`settleFor`) and `internal/sim/sim_test.go` |
| That the original **does** remove the observer's entry when its connection dies | `sub_1400E52D0:227-242`; section 7 of the intentional file |
| **Which connections set `conn+88` to 1**, and whether the binary nests the writers wrongly or hides one | **Both writers are on a first connect**, and there is no third: `sub_1400EB5F0:469` (session known, no peer for that secret) and `:490` (`v13 == 0`, no session at all). The premise was a misreading of which `if` the `else if` binds to — `:464` closes `if (v15 != 0)` and `:485` closes `if (v13 != 0)`. Exhaustive over `src/`: the only other byte-sized store to `+88` writes **0** into a `std::string`. The field means "has completed its `LobbyConnect`" and the gate's drop path is unreachable for a connection that speaks the protocol, so it left the divergence register. `UNINTENTIONAL-DIVERGENCES.md` 10.2, [`PERFORMANCE.md`](PERFORMANCE.md) 5, journal **§45.2** |
| Whether the journal's `§5` exists | **It does not, and it will not.** The text is `### Cause` / `### Fix` / `### Regression test` under `## 4.`; the index row was corrected to say so instead of a `## 5.` being created. Journal **§45.1** |
| Whether `-maxPagesToStorePerSession` should get a cap | **It should not**: the original has none and neither do we, so the ceiling is the operator's on both sides. Journal **§51** |
| That `Session.Peers` is a slot table and not a map, and **why** | `SESSION-JOURNAL.md §40.2`, `PERFORMANCE.md` 4.3 |
| That `peer+68` is the `protocolVersion` of the `LobbyConnect`, and not a state counter | `sub_1400EB5F0:470` and `:491`; `INTENTIONAL-DIVERGENCES.md` 2.1 and 7 |
| That the message built by `sub_1400E2590:360` is the `0x0A` **for the peers**, and that in `src/` there is no type-8 class | `MultiGameDataMessage__vmethod_18` serialises with type 10; section 7 of the intentional file |
| Whether the beacon should keep its **second port, its `0x20` message, its per-interface addresses and its `0` disable**, none of which the original has | **Open.** The original has one port (9999), one address (`255.255.255.255`), one format (`0x21`) and no disable, all cited at `relay.md` and `UNINTENTIONAL-DIVERGENCES.md` 13. It needs a **decision**, not a measurement: `0x20` has no code in `src/` at all. The measurement that would make the answer stick is a UDP listener in the harness |
