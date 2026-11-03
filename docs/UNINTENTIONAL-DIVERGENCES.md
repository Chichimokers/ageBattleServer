# UNINTENTIONAL divergences: open bugs and unexplained gaps

> **What this file covers.** Everything known or suspected to **differ from the original
> without having decided it**, plus the fields that are still marked `UNKNOWN` in the
> audit. That is: open bugs, gaps, and questions with no answer.
>
> **The criterion, which is the same as in the other file.** An entry here is a difference
> **observable from outside** —one a client, a peer or the replay host can tell on the
> wire— **without a written reason for accepting it**. An `UNKNOWN` also fits, and then
> what is recorded is that **it is not known**, not that it is known and accepted.
> Anything that is only an internal decision is neither here nor in
> [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md): it is in
> [`PERFORMANCE.md`](PERFORMANCE.md) and in [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md).
>
> **Status.** Current. It is the "this is a bug" half of the divergence register; the other
> half, "we chose this", is in
> [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md). **The two files
> are mutually exclusive: no finding appears in both.** And the criterion of the split is
> only one: if there is a **reason to accept** the difference, it is intentional; if there
> is none, it is here. A "do not touch" with no written reason **does not** count as a
> reason.
>
> **Evidence.** Every row cites the binary (`src/sub_*.cpp` with a line) or a capture, or
> declares that there is neither —which is a fact, not an excuse—. Where the evidence is
> a section of the journal it is cited as `SESSION-JOURNAL.md §N` and the original text is still
> there.
>
> **How something enters and leaves this list.** It enters with the measurement or with the
> `UNKNOWN` written down. It leaves with a commit and a test, and moves to the
> intentional file if the difference is kept on purpose, or is closed as a
> non-divergence if it was ours.

## 0. The criterion applied to every entry, 2026-10-05, and what it moved

`SESSION-JOURNAL.md` §42 tightened the criterion to the black box and then did not apply it to the
entries that were already filed. This is that application, entry by entry. **Nine of the
fifteen entries failed it**, and the reason each one failed is in its own section with its
evidence, so this table is an index and not a substitute. **10.2 was re-examined the same
day, after this table was written**, and it is the one row whose verdict changed: its
premise did not survive the check, and it is now closed.

| entry | visible from outside? | what happened |
|---|---|---|
| 1. the replay upload's byte range | **yes**: it is the body of a POST | **moved** to [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) 6: the original does not upload, so there is nothing to be identical to. Measured by the user with a listener, `-uploadVia=lobby`, and a match played to the end |
| 2. the extra bsPort peer | **yes**: the pending list in the `0x0A`, the relays | **stays**, blocked on three real clients |
| 3. the extra `LobbyRelay`s and the missing acks | **yes** | **stays**, blocked on the same capture as 2 |
| 4. the `LobbyConnect` 8+4 bytes | **yes** | **stays**, blocked on a reconnection capture |
| 5. the "WebSocket ping" | **no**: `0x65` and `0x6B` are HTTP header bytes of the `101`, read out of its second TCP record. No capture has a `0x89` or a `0x8A` on either side | **closed as a false claim**; the real difference it exposed (a missing `Server` header) was fixed and is now compared |
| 6. the observer step and a fresh activation | **not a difference of ours**: the step is the original's arithmetic, and the binary has no arm that could serve the activation itself | **closed by reading the binary**: the original does not serve it in the same tick, and the 1,125 ms it costs is now measured and is what the harness's settle has to cover |
| 7. the reconnected case | **yes** if it happens: the echo bytes, the close, the resend | **stays**, blocked on a real reconnection |
| 8. two observers sharing a `peerIdentifier` | **yes** if it happens | **closed by reading the binary**: the comparator is one line (`sub_1400E43E0:43`, `sub_1400E46C0:25`) and the first match wins, which is what the code does |
| 9. the top-level `0x01` | **yes** a client could see it | **closed by fixing it**: the handler is gone and `0x01` lands in the `default` |
| 10.1 `session+2140` | **no**: its only readers are itself and the uploader's own latch | **moved** to [`PERFORMANCE.md`](PERFORMANCE.md) 5 |
| 10.3 the four counters | **no**: exactly the munin and the state dump read them | **moved** to [`PERFORMANCE.md`](PERFORMANCE.md) 5 |
| 10.2 `conn+88` | **no, once the premise was checked**: the field *is* read by the dispatcher's own gate (`sub_1400DE050:136`), but the gate's drop path is only reachable for a connection whose **first** message is not a `LobbyConnect`, and every connection of every capture sends its `0x15` first | **closed by reading the binary** (`§45.2`): the premise was a misreading of the nesting, **both writers are on a first connect**, and there is no third writer. Kept below, because the reconnect branch never writes the byte and that is `PENDING.md` 2.2 |
| 11.1, 11.2 the two harness flakes | **no**: a test that fails on some runs is not anything a client can see | **closed** by fixing the cause (11.1: the settle was shorter than the observer step's period) and by replacing the assertion with a deterministic and stricter one (11.2) |
| 12. the ceiling of the instrumentation | **no**: it is a limit of what can be measured | **moved** to [`CAPTURES.md`](CAPTURES.md), "The ceiling of the instrumentation" |

**The other file needed no move.** Every entry in
[`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) is either observable on the wire
or is explicitly a "this is not a difference" note, and each one carries a written reason,
which is the half of the criterion that puts something there. Section 6 of that file, whose
heading said "what is NOT a divergence", holds three wire-visible differences that are
accepted with a reason (the grouping of the `0x0A`s, the 4 extra acks of second 26, and the
probe) and its heading is now accurate about that.

Ordered by what it costs to resolve, not by age.

---
## 1. What byte range is uploaded to the replay host

**Moved to [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) 6 on 2026-10-07.** It
is **no longer a bug**, and the reason is not that the answer was found: it is that **the
original does not upload**. The user ran it against a listener with `-uploadVia=lobby`, and the
TCP connection arrived and **no request ever followed** — neither in a short run nor with a
match played to the end. `battle-server.txt` agrees with the only recorded run of the original:
the uploader is built (`:18`) and the connect is *deferred and never completed* (`:23`), with no
request in the remaining 423 lines.

The entry stays here in the register's own words so that nobody re-opens it as a bug, and the
argument that made it open is preserved below. **What the question was** is still the right
question to ask of any byte range: *is this the body or the whole message as it came off the
wire?* What changed is that with no request from the original there is no answer to compare
against, so the difference is accepted with a reason instead of being chased.

### 1.1 What was measured, and what the question was

- **Status when it was open**: open, and it was the only doubt left about the replay upload.
  Observable: what reaches the replay host is a POST, and its body is bytes.
- **What diverges**: we upload **only the body**, from the type byte onwards. If `a2` is the
  buffer of the current message, what the original would upload is the **whole message as it
  came off the wire**, framing included.
- **Why it could not be closed by guessing**: the call to the uploader passes the stream buffer
  and its size **separately** —`(*(**a2 + 16))(*a2, 0)` and `(*(**a2 + 32))(*a2)`, with the
  vtable convention of these streams (16 = Read, 32 = Size, 48 = Seek, 64 = Write),
  read at `sub_1400DE050:517-518` and passed at `:529`— and the length parameter that
  reaches the uploader is `(*(**a2+32))(*a2)`, that is the **total size**, not a slice. On
  top of that there is a third size argument, `v27 = (v23[1] - *v23) >> 3`
  (`sub_1400DE050:519`), which is the length in 8-byte words of the **accumulated packets
  vector**, and whose role inside the uploader is not traced. **And it cannot be closed
  without a real POST**: guessing wrong corrupts the replays, which is exactly the data
  that cannot be recovered afterwards.
- **How it would have been resolved**: a real POST against a replay host, or the logs of one
  that was already done. **Tried**: a real listener against the original. It never came.
- **What was already established** (read out of the binary,
  `WWBS::WorldwideReplayUpload__vmethod_30`, `0x140110f80`, and `ReplayUploadManager`): it is a
  **raw POST**, not `multipart/form-data` (that string is at `sub_1401012B0`, RLink's
  general HTTP client); the line is built at `vmethod_30:26-47` and is
  `POST /game/replay/setReplayData?matchid=<u64>&sequenceid=<u32> HTTP/1.1`; `matchid` is the
  **sessionID** (`sub_1400E7500:33`: `*(u64*)(batch+0x10000) = *(u64*)(session+2104)`, and the
  sessionID lives at `session+2104` with the titleID at `session+2096`: `sub_1400E1DE0:33`
  copies both from the key and `sub_1400E9E90:289-290` prints them in that order); `sequenceid`
  is a counter **per session that starts at 1** (`sub_1400E7500:32` and `:34-35`,
  `++*(u32*)(session+2136)`); the body goes through a `ConvertBinaryToString` with the log
  `"OnAddReplayData - ConvertBinaryToString"`; there is a state guard
  (`WWBS::...vmethod_30:24`, `if ((u16)(state - 6) <= 4)`, it is only sent with the state in
  [6, 10], otherwise the batch is queued); and the body buffer is **inline 64 KB** at
  `batch+65552`, with the length at `batch+131088` (`vmethod_30:59` and `:46`). All of that
  we already do correctly. **Only the range was in question.**
- **Evidence**: `SESSION-JOURNAL.md §16.2`, and `§52` for the move.

**Update 2026-11-03: the uploader has since been removed from this implementation.** The
server no longer opens the replay socket at all and the flags are accepted and ignored, so
the question above no longer applies to this tree. See
[`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) 6 ("The replay socket is never
opened") and 7.

---

## 2. On bsPort we add a second peer and the original does not

The most expensive finding still open. Localised as far as the evidence reaches, and
explicitly **not** touched in the server.

- **What is measured**: 31 extra `LobbyRelay`, extra acks, and `fe 00 02 01 02 00 00` against
  `fe 00 01 01 00 00` in the pending list of the `0x0A`. The original stays at **a single
  peer**; we are at two.
- **What is already established, from the binary and without inferring from names**:
  - **`sub_1400E5670`** is the only function that writes a peer into the session's array
    (`session + 24 + 8*peerID`, line 100), and it is reached in the binary by a single chain:
    the ADDPEER case of `sub_1400DAAF0` (`:298`) calls `sub_1400EAE60`, whose only caller is
    that same case, and it calls `sub_1400E5670` on its line 29. Before creating it looks for
    the **global id** in the session's tree (`session+2080`, `:59-61`) and **returns without
    creating if it is not there** (`:75-85`).
  - That tree is written by **one single** function, `sub_1400E5B00` (`session+2080`, `:23-24`),
    and its only caller is the REQUESTJOIN case of the same switch (`sub_1400EA680:259`).
  - **`sub_1400EB5F0` (the `0x15` handler) adds no peer** on any of its
    paths. That is where we add them, and that is where the divergence is.
  - The client **does not** send either of the two as a top-level `0x0F`: it puts them inside
    a `0x13` and the original dispatches them recursively (`sub_1400DE050:567`). The first
    ones come **zlib compressed** (flagBits `0x04`, `sub_1400DAAF0:75`): without
    inflating them the internal type is `0x78` and no peer message is visible at all.
- **What is NOT known**: **which of the two messages the original rejected**, and it cannot
  be known with this capture. The two messages of peer 2 go in **different connections** (the
  REQUESTJOIN on 50316, the ADDPEER on 19850), the proxy stamps **whole seconds**
  (`cmd/proxy/packets.go:47`), and each connection is read on its own socket: **the order in
  which the original processed them is not in the capture**. If the ADDPEER was processed
  before the REQUESTJOIN wrote the tree, the refusal is the code's (`E5670:75-85`); if it was
  processed after, the refusal is **another** one, and it is not identified.
- **Why the server is not touched**: adding the peer in the `0x15` (where we are), removing
  it from there and putting it in the `ADDPEER`, would be a large change on top of a question
  missing the fact that decides it. And **a change that swaps one visible absence for
  another is not a fix**.
- **What has been done**, in the harness, because it costs zero seconds of game time: the rule
  **`[pending-peers]`** reads the pending list from each side and reports when they name
  **different sets** of peers. It compares **sets** and not **counts**, and this was
  measured: in `2026-09-29` both sides name `{1}` and only how many acks carried each
  list changes, which is grouping and not peers. A rule that compared counts would have
  reported a peer divergence in a capture where there is none, and it did it on **all three**
  connections. The **13** peer messages can be read from the captures, and only if the
  compressed ones are inflated
  (`TestTheFirstPeerMessagesAreCompressed` fails if the un-inflated byte ever reached being a
  peer type).
- **How it would be resolved**: a LAN match with **three or more real clients on bsPort**,
  with the game session started (so that the `0x0A`s exist) and **no observer**. With two
  connections, "one peer per connection" and "one peer in total" are **indistinguishable**;
  with three, if the original stays at 1 or goes up to 2, the question answers itself. If
  additionally the third client's `ADDPEER` arrives **over the same connection** as its
  `REQUESTJOIN`, the order is observable and it tells us which of the two messages was
  rejected. In order of value: (1) three or more clients on bsPort; (2) the **destination
  bytes** of each `LobbyRelay`, not just the count, because the distribution needs a non-null
  slot in `session + 24 + 8*toPeerID` (`sub_1400E6160:18-19`) and with the count alone you
  do not know which peer each one points at; (3) **`0x0F`**, which has been needed since
  before.
- **Evidence**: `SESSION-JOURNAL.md §27` and `§29`. Files: `internal/sim/peers.go`,
  `internal/sim/relaypeer.go`, and the tests `TestTheRuleStaysQuietWhenOnlyTheCountsDiffer`,
  `TestTheOriginalNamesOnlyPeerOne`, `TestTheFirstPeerMessagesAreCompressed`.

## 3. The extra LobbyRelays and the missing acks of the bsPort capture

- **What is measured**: **25 extra LobbyRelay** to the second client, and **5 fewer acks** in
  the player's connection.
- **Why it has not been touched**: they are informative and no capture separates them from the
  tick cadence without another trim of the clock. That capture's report says, with numbers,
  that its sustained rates are the same (9/s on all three sides) and that the seconds that
  differ **are not the first one**: "no cause is claimed for it".
- **How it would be separated**: another trim of the clock, or the same trim of the clock as
  §2.
- **Note on the relation with §2**: both things were in the same capture and they are **distinct
  questions**. This one is about distribution; §2's is about why there is an extra peer.
- **Evidence**: `SESSION-JOURNAL.md §27` ("What is left of this session") and `§28` ("What stays
  open").

## 4. The `LobbyConnect` carries the same 8+4 bytes on both connections

- **What diverges**: our `LobbyConnect` carries the same 8+4 bytes on the player's connection
  and on the observer's; **the original varies them per connection**.
- **What is known and what is not**: for the **observer** our bytes are the same as
  its, that is, there **is nothing to fix** there. On the **player's** they do diverge.
- **Why it is here and not in the intentional file**: because **there is no written
  reason for accepting it**. The only thing on record is "the player works fine", which is an
  observation that it does not break, not a reason the difference is accepted. And the call
  is on the reconnect path, which is exactly the path with no committed capture
  (see §6).
- **How it would be resolved**: a capture with a reconnection (row 6 of the `CAPTURES.md`
  matrix).
- **Evidence**: `SESSION-JOURNAL.md §25` ("The only thing left different, and not touched").

## 5. Closed on 2026-10-05: the "WebSocket ping" was never a ping

**It was never a divergence. It was a misreading of the harness**, and this section is the
record so that it does not come back.

- **What it claimed**: that we send a **WebSocket ping** (`0x65`) once per connection where
  the original answers with a **pong** (`0x6B`), measured in the four WebSocket logs as two
  `0x65` rows server -> client and no `0x6B`.
- **What those two bytes are**: the original's `101 Switching Protocols` answer arrives
  **split in two TCP writes** — 86 bytes and then 70, measured on
  `2026-09-30_010531_a553ef8fd63c` — and the harness read the **tail** as a separate record
  and reported its first byte as a message type. `0x65` is the `e` of
  `Sec-WebSocket-Accept` at the head of **our** second record; `0x6B` is the `k` of the
  accept key at the head of **the original's** second record. Both are HTTP header
  continuation bytes of the same `101`. **Neither side ever sent a control frame.**
- **Measured over the five committed captures, as a control**: scanning every logged row of
  both sides of all five for a first byte in `0x80..0x8A` that is not a binary frame, the
  only opcode that appears is `0x88` (close) on the two bsPort captures, and **there is no
  `0x89` (ping) and no `0x8A` (pong) anywhere**, on either side of any capture.
- **The difference that was really there, and is fixed**: our `101` carried **no `Server`
  header** and the original's carries `Server: WebSocket++/0.8.2`. It is sent now
  (`internal/server/ws_listener.go`, `ResponseHeader`) and **the whole upgrade response is
  compared** by `ruleUpgradeResponse`, header set and status line, with each side's
  `Sec-WebSocket-Accept` checked against the derivation of the key **that side's** request
  carried.
- **Falsification**: `TestUpgradeResponseCatchesTheMissingServerHeader` feeds the reader the
  two real responses (the original's four headers, ours with three) and fails if the rule
  says nothing; `TestUpgradeResponsePassesOnceTheHeaderIsThere` is the other direction;
  `TestUpgradeResponseDoesNotCompareTheTwoAcceptStrings` and
  `TestUpgradeResponseCatchesAWrongDerivation` keep the rule from failing on correctness.
  `TestUpgradeHeadersReadsSplitRecords` pins the two-record shape that caused the
  misreading.
- **Evidence**: `internal/sim/upgrade.go` (`stripHandshake`), `internal/sim/upgrade_test.go`,
  `SESSION-JOURNAL.md` §44.3. The ping claim itself came from `SESSION-JOURNAL.md §25`.

## 6. Closed on 2026-10-05: the original does NOT serve a freshly activated observer in the same tick

**It was an `UNKNOWN` about the original, and it is now answered.** This section is the
record, so that the answer is not rediscovered and so that nobody "fixes" the step to make a
fixture deterministic.

- **The question, without softening it**: if an observer that **activates fresh** is served
  by the original **in the same tick** in which it activates, or if it **waits for its turn**
  at the step.
- **The answer**: it **waits**, and there is **no arm in the binary that could say
  otherwise**. `sub_1400E43E0` — the ActivateObserver handler, reached from the `0x1A` case
  of the dispatcher through `sub_1400EC580`, which passes the connection straight through —
  walks the observer list (`:39-48`), writes `entry+12 = 1` (`:49`) and builds **one**
  message, the `ActivateObserverResponse` whose wire type is 30 (`:54`, `:116-142`). It
  **never** calls `sub_1400D5560` or `sub_1400D4A70`, which are the only two functions in
  the binary that fetch frames out of the store and write them to an observer; the only
  callers of those are `sub_1400E4EC0:79` and `:83` — the tick's observer step, called from
  `sub_1400E2590:97` — and `sub_1400E3260:43`, which is the reconnect resend.
- **And inside the step nothing distinguishes a new arrival**: the body opens at `:62` with
  the active flag (`if (*(BYTE *)(v4 + 12) != 0)`) and the cursor picks the entries
  (`:57`, `:199`, `:203`); the cursor advances on every tick whether it visited anybody or
  not. There is no "it just activated" arm to find.
- **What it costs, and this is the part that had not been measured**: the cursor at
  `session+2296` takes `interval+1` values (`sub_1400E4EC0:206-209` wraps only when it is
  **strictly** greater than the interval) and index 0 is served on the ticks where the cursor
  is 0. So a freshly activated observer waits **up to `interval+1` ticks** — **9 ticks,
  1,125 ms** at the defaults. The **worst phase is 1 and not `interval`**: from `interval`
  the cursor wraps to 0 on the very next tick, from 1 it has to climb all the way round.
  Measured by `TestTheWorstPhaseCostsTheWholePeriod` and
  `TestAFreshlyActivatedObserverIsServedWithinOneStepPeriod`
  (`internal/session/session_test.go`), which walk every phase there is.
- **What that cost the harness, and it was not a fixture's fault**: the replay's settle was
  three simulation periods (375 ms, floored at 400 ms), which is **less than a quarter** of
  the step's own period. With the phase that puts the first visit 9 ticks out, the replay
  closed the connection before the frame was written and `replay-page-order` came out
  **must-match with no frame in it**. The settle's floor is now `interval+2` ticks, read
  from the server's own `ObserverUpdateIntervalTicks`, and the report carries the arithmetic
  as a deviation. See §11.1 for what that closed.
- **Evidence**: `sub_1400E43E0:39-49`, `:54`, `:116-142`; `sub_1400EC580`; `sub_1400E4EC0:54-62`,
  `:79`, `:83`, `:199`, `:203`, `:206-209`; `sub_1400E2590:97`; `sub_1400E3260:43`;
  `internal/sim/replay.go` (`settleFor`, `observerStepPeriod`). `SESSION-JOURNAL.md` §44.4.

---
## 7. The reconnected case: modelled, and with no capture that validates it

None of the five committed captures contains a reconnected peer. Everything in this block
is modelled by reading the binary and **not checked** against the original's bytes.

- **`peer+104`, the sync-message counter, reaches the echo** — modelled
  (`a6eb4d6`). `sub_1400E2E00:202` increments it and its only reader in the whole binary is
  `sub_1400EB5F0:341`, which puts that number into the `dataReceivedCount` of the
  `LobbyConnect` it returns, over a **clone** of the client's message (`EB5F0:306`, `(*v33)[4]()`).
  Before, we returned the client's body as it was. **What is not known**: whether the client
  cares.
- **CLOSED on 2026-10-06: the count guard is now modelled in full, and it was a wire-visible
  gap.** The other half of the guard (`session+2141 == 0 || peer+90 != 0`, `E2E00:35`) was
  **not modelled at all**: the sub-message was queued whatever the peer was, so a peer that
  joined **after** the frame store was created, or one that had sent LeaveSimulation, had its
  data put into the next `0x0A` — a sub-message the server never sends. It is the `v4 != 0` and
  the `(*(a1+2141) == 0 || *(v4+90) != 0)` of the same block, and **the guard wraps four
  effects, not one**: no queue, no `++` of the counter, no "this peer has sent data" mark and
  no last-sync stamp. The counter now lives inside `Session.QueueGameData` for exactly that
  reason; a caller that incremented before asking is what the code used to do.
  **Why it is closed without a capture**: all five committed captures replay with **no
  must-match divergence** before and after (`internal/sim`, 770.3 s), because none of them has
  a late-joining peer uploading into a recording session — so the change is a no-op on the
  recorded evidence and the evidence does not contradict it. What it is **not** is verified
  against a recording of the case, and that still needs one.
- **A limitation of the model that is known and has to be said**: the original's serializer
  writes that field for **all** states except 2, with `-1` inside
  (`LobbyConnectMessage__vmethod_20:59`), so its state-0 and state-3+ echoes are four
  bytes longer than the requests our parser accepts. Only the shape of state 2 is
  reproduced, which is the one that round-trips; the other two still return the request as
  it was. (The state is **one byte** on the wire, not a u32: `:48-49` reads a byte at
  offset 17 and stores it as a dword, and the 4-byte field only exists on the state-2
  path, `:50-56`.)
- **Whether the original would also close the peer's socket, or only the replaced
  connection's**: the branch has only one call to `sub_1400D4860` and it is on the connection,
  so **from the binary** it is only that one. Without a capture it cannot be ruled out that
  the client would notice the close.
- **The reconnection states that resend frames**: only state 2
  (`sub_1400EB5F0:237-249`). This part is implemented.
- **How it would be resolved**: a real reconnection (row 6 of the `CAPTURES.md`
  matrix): kill and reopen the match halfway, looking at the `secret` and that it keeps the
  slot (`6884fa3`).
- **Evidence**: `SESSION-JOURNAL.md §30` ("What cannot be established") and `§36.2`.

---

## 8. Closed on 2026-10-05: two observers sharing a `peerIdentifier` — first match wins

**This was open here for a bookkeeping reason and not for a missing fact: the behaviour was
settled in `SESSION-JOURNAL.md §31` and nobody wrote the binary reading down.** It is recorded now.

- **What was undecided**: the pairing condition when **two observers share a
  `peerIdentifier`**.
- **What the binary says**, and it is one line used twice.
  `sub_1400E3B50:240` writes the request's third u64 into the **connection's** own offset 0
  (`*(_QWORD *)a2 = *(_QWORD *)(a3 + 64)`), and the entry it appends holds the
  **connection pointer** at `+0` (`:239`, `:245-254`). Both lookups then compare that
  pointer's own offset 0 against the caller's: `sub_1400E43E0:43`
  (`while (**(_QWORD **)v4 != *a2)`) and `sub_1400E46C0:25`, which are **the same line**.
  **First match wins**, for activation and for removal alike, and the entry's `+0` is a
  pointer, so with two observers sharing an identifier the second one arms the first entry
  and a `StopObserve` from it removes the first one. The entry that gets armed and the
  connection that gets the answer are two different objects in that case — which is the
  original's behaviour and not an accident of this port.
- **Why no capture was ever needed for it**: the comparator is the whole question, and it is
  in the binary. What a capture would add is whether two instances of the client ever
  really do send the same identifier, which is a fact about the game and not about the
  protocol.
- **What was here for**: the entry said the pairing "cannot be decided here", which was
  true of a **capture** and false of the question.
- **Evidence**: `sub_1400E3B50:239-254`, `sub_1400E43E0:39-49`, `sub_1400E46C0:25-33`;
  `internal/session/observeridentity_test.go`
  (`TestSameIdentifierArmsTheFirstEntryAndAnswersTheCaller`,
  `TestStopObserveRemovesTheFirstEntryOfASharedIdentifier`,
  `TestZeroIdentifierIsAnIdentity`), and the comment on `Session.ActivateObserver` in
  `internal/session/session.go`. `SESSION-JOURNAL.md §30` and `§31`, `SESSION-JOURNAL.md` §44.4.

---

## 9. Closed on 2026-10-05: a top-level `0x01` is an unknown message type

**This was open here. It is no longer a divergence and this section is the record of
why**, so that it does not come back as a finding.

- **What it was**: a **top-level** `RequestJoinMessage` (`0x01`) created a session,
  registered a peer and emitted an `AddPeer` (`internal/server/handlers.go`,
  `handleRequestJoin`). A client that sent one got a whole match out of it.
- **What the original does**: nothing. Its dispatcher's case labels are `0x09, 0x0F,
  0x10, 0x13, 0x14, 0x17, 0x18/0x1C, 0x1A, 0x1B, 0x1D, 0x1F` (`sub_1400DE050:266` onward),
  plus the `0x15` handled **before** the switch (`:136-154`), and the `default:` arm
  (`:948-1026`) has no other statement than the log
  (`"!!!Unknown message type %d received!!!"`, `:948`). It returns 0 and closes nothing.
- **Why nothing else in the binary does it either**: `sub_1400EA680`, the one function
  that looks the session up or creates it and writes the `session+2080` tree, is reached
  from **one** place — the type-2 case of the **internal** peer switch,
  `sub_1400DAAF0:193` — which is itself reached from the `0x0F` case (`sub_1400DE050:399`)
  and from the recursive dispatch of the `0x13` (`:567`). There is no path from a
  top-level `0x01` to it. The finding that the game never sends one (`_7`) is a negative
  measurement: it says the path is not exercised, not that handling it is right, which is
  why it could not be the reason to keep it.
- **What changed**: the case and the handler are gone, so `0x01` lands in the `default`
  the way the original's does. The connection is left open, because the original's
  `default` arm does not close anything.
- **Evidence**: `TestTopLevelRequestJoinIsAnUnknownMessageType` and
  `TestTopLevelRequestJoinIsNotOneOfTheGateTypes`
  (`internal/server/handlers_test.go`), commit `ed5ff38` in `src_go`. The first fails on
  the previous code with "a top-level 0x01 created 1 session(s)".
- **What was careful about**: twenty tests used the handler as a scenario fixture. They
  now use `testSessionWithPeer`, a test fixture that creates the session and the peer
  slot and deliberately does **not** emit the `AddPeer` (the original's internal
  `REQUESTJOIN` emits none either: `sub_1400EA680` returns after `sub_1400E5B00`).
  `wsJoinHost`, which built a session over WebSocket with a `0x01`, now builds it with a
  `LobbyConnect`, which is the path the game really takes.

---

## 10. The field-by-field audit, and what is left of it

The field-by-field audit of the session object and the connection object (`SESSION-JOURNAL.md` §32,
`§32.5`, `§32.8`) left eleven fields unexamined and settled all but these. **"We do not
read it" is a finding and not a gap** when it is known that what lives there is metrics;
what follows is what is **not** known.

> **Three of the six left this file on 2026-10-05, and none of the three was ever a
> divergence.** `10.1` (`session+2140`, the "already published" latch) and `10.3`
> (`session+2176`, `+2224`, `+2232`, `+2240`) are munin counters and the state dump, **their
> readers are exactly `sub_1400E9E90` and `sub_1400EBE60` and neither travels on the wire**,
> so under the criterion at the top of this file they cannot be divergences at all: no client,
> no peer and no replay host can tell anything apart. They were filed under the older, looser
> criterion. **The audit that produced them is still the evidence and nothing is lost**:
> what each one is, what writes it and where it went is in
> [`PERFORMANCE.md`](PERFORMANCE.md) 5 ("The counters that reach nothing"), the record of
> the audit is in [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §32.5 and §32.8, and the open
> question of naming them stays on the work list in [`PENDING.md`](PENDING.md) 2.1 and 2.3.
>
> `10.2` (`conn+88`) was closed on the same date, and it is **kept below with its reading**
> rather than removed, because the reading is what closed it and the consequence of it is
> still open. Its question was **which connections set it**, the premise it rested on turned
> out to be a **misreading of which `if` an `else if` binds to**, and the answer is that
> **both writers are on a first connect**. It leaves the register for the other reason: the
> gate's drop path is **unreachable for a connection that speaks the protocol**, so there is
> nothing a client, a peer or the replay host can tell apart. What the field is and what it
> gates is in [`PERFORMANCE.md`](PERFORMANCE.md) 5 ("`conn+88`, settled"), the reading with
> its lines is in [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §45.2, and the **one** thing the
> binary cannot close — a reconnect, whose branch never writes the byte — is on the work list
> in [`PENDING.md`](PENDING.md) 2.2.

### 10.2 `conn+88`: CLOSED on 2026-10-05, settled from the binary, and never a divergence

The question this entry carried was **which connections set `conn+88` to 1**, and it is
**answered**. It is kept here, and not deleted, because the answer is what closed it and
because one consequence of it is still open.

- **The premise of the entry was wrong, and it is a reading error, not a nesting error.**
  The two writers (`sub_1400EB5F0:469`, `:490`) are **not** under `if (v15 != 0)`
  (`EB5F0:131`). Brace depth, which is the only thing that decides this: `:464`'s `}`
  closes `if (v15 != 0)` (opened `:132`), so `:465`'s `if (v16 == 0)` is its **sibling**
  and `:485`'s `}` closes `if (v13 != 0)` (opened `:127`), so `:486`'s `else if` is the
  **`else` of `if (v13 != 0)`**. The decompiler's braces are right. `:469` is under
  `v13 != 0 && v15 == 0` — the branch for **no peer found for that secret** — and `:490`
  is under `v13 == 0` - **no session at all**, which is the first connect.
- **`v13` is the session, and that is positive and not an assumption.** The lookup at
  `:87-111` searches a tree on the composite key **(`*(_WORD *)(a3 + 48)`,
  `*(_QWORD *)(a3 + 56)`)**, which are the `LobbyConnectMessage`'s **`titleID` and
  `sessionID`** (`MESSAGES.md:149-150`), and yields `v11[6]` - or **0** when the key is not
  there (`:110`). And `:506`'s string, reached exactly when `v13 == 0`, names the same thing:
  `"Attempt to reconnect to a non-existent sessionID"`.
- **So the answer is: both writers are on a first connect.** `:490` is the first connect
  outright, and `:469` is the one where the session is already known but the secret has no
  peer yet, which is also a first connect. `v16` is `*(_DWORD *)(a3 + 80)` (`:130`), so
  `:465`'s `if (v16 == 0)` and `:486`'s `*(_DWORD *)(a3 + 80) == 0` are **the same
  condition**, and both writers require the LobbyConnect's dword at `+80` to be 0, i.e. a
  new connection and not a reconnect. `:506`'s string names what the remaining case is:
  `"Attempt to reconnect to a non-existent sessionID"`, reached when `v13 == 0 && a3+80 != 0`.
- **There is no third writer.** Every byte-sized store to `+88` in all 7,450 files of `src/`
  is three lines: `EB5F0:469` and `EB5F0:490` (both `= 1`) and `sub_140233400:61` (`= 0`).
  That third one is **not a connection**: `sub_1400CD030(v3, 8, (const char *)(a2 + 88))`
  (`sub_140233400:46`) passes `a2 + 88` as the **`const char *`** of a `std::string`, whose
  capacity is at `+112` (`:47-48`, the `>= 0x10` heap test) and is set to **15**, the
  small-string marker, at `:60`. It is a `std::terminate` helper
  (`"termination_handler call failed"`, `:31`). The hypothesis that the field is 1 without a
  store is **dead**: a zero-initialised object is 0, and the only writes of 1 are those two.
- **What the gate does, read whole** (`sub_1400DE050:135-247`): read the message type; if
  `conn+88 == 0` **and the type is not 21, return 0**; otherwise handle the LobbyConnect and
  fall through to the normal dispatch at `:248`. `21` is `LobbyConnect`
  (`MESSAGES.md:60`, `0x15` = 21 decimal). So the field means **"this connection has
  completed its LobbyConnect"**, and the dispatcher drops everything else until it has.
- **The reader at `EB1E0:33-34` was a different object.** There, `a3` is the **message**
  (`a2` is the connection: its `+8` is printed as `peer=`), and `a3 + 88` is the removing
  peer's id, used as `session[24 + 8*id]` — the peer slot table. Same offset, not the same
  field. It is what made the entry look like two independent confirmations of a layout.
- **Why it is not a divergence.** Reaching the difference needs a connection whose **first**
  message is not a LobbyConnect. Every connection of every committed capture sends its
  `0x15` first, **including both observers** (they send `0x15`, then `0x1C`, then `0x1A`),
  so the drop path is never taken, the original's and our behaviour on the wire are the
  same, and **nothing is compared differently**. The gate is **not implemented and does not
  need to be**: implementing it wrongly would reject the observers of `2026-09-29_012528`,
  and implementing it *rightly* would change no byte of any capture.
- **What is still open, and it is not this**: the **reconnect** branch (`v13 != 0 &&
  v15 != 0 && a3+80 != 0`, `EB5F0:133-463`) **never writes `conn+88`**, and that branch is
  the one that swaps the peer onto the new connection (`:137-139`). So a reconnect can only
  work if the connection object already carries the byte. That is not a question about a
  byte's writer; it is [`PENDING.md`](PENDING.md) 1.5, a real reconnection, which no capture
  has.
- **Evidence**: `SESSION-JOURNAL.md` §45.2 (the reading, with the depths), §32.5 and §32.8;
  `sub_1400EB5F0:126-131`, `:464-465`, `:485-486`, `:506`; `sub_1400DE050:135-139`,
  `:185`, `:248`; `sub_1400EB1E0:30-34`; `MESSAGES.md:60`; `trimsim -inspect` on the five
  committed captures, whose `0x15` is the first message of every connection.

---

## 11. Closed on 2026-10-05: the two harness flakes, and why they were never divergences

**Neither was a divergence and both are closed.** They were measured pending work about
the harness, they are recorded here so that the measurement is not lost, and they leave
this file for the same reason §10's counters did: a test that fails on some runs is not a
difference a client, a peer or the replay host can tell on the wire. The current state of
each is in [`PENDING.md`](PENDING.md) 5 and the evidence is in
[`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §44.4.

### 11.1 `TestSim_synthetic`: 1 in 60, and it was the harness's own settle

- **Signature**: a `replay-page-order` must-match with the `0x19` and the `0x1E` and **no
  frame** from the observer stream. The `0x19` and the `0x1E` arriving is the signature of
  "the observer was served nothing", not of a rejected observer: a rejection does not send
  the `0x1E`.
- **Measured**: 1 in 60 replays of `testdata/synthetic`, and **pre-existing** — it
  reproduces identically on `ed68bca`, the commit before the peer table became an array.
- **The mechanism, and it is not the fixture's length**: the observer step is a **phase** of
  `interval+1` ticks (see §6), so an observer that activates at the end of the recording can
  wait **9 ticks, 1,125 ms** for its first visit, and the phase it lands on depends on where
  the session's cursor was. The replay's settle was **400 ms** — three simulation periods,
  floored — which is **less than a third of that period**. On the phases where the visit came
  after 400 ms the replay **closed the connection before the frame was written**, and the
  harness reported its own truncation as a divergence of the server.
- **What changed**: the settle's floor is `interval+2` ticks, derived from the same
  `ObserverUpdateIntervalTicks` the session is configured with, and the report prints the
  arithmetic. Enlarging the fixture would **not** have fixed it, and that is worth saying:
  the settle is always derived from the last whole second of the recording, so a fixture of
  any length keeps landing on the same 1-second tail and the same phase coin-flip.
- **Falsification**: with the floor removed,
  `TestTheSettleCoversTheObserverStep` fails with "settle = 400ms, but an observer
  activated at the end of the recording waits up to 9 ticks (1.125s) for the step's first
  visit", and `TestTheSettleIsExplainedInTheReport` fails with no deviation explaining it.
- **What was NOT touched**: the step. It is the original's arithmetic and it stays
  (`internal/session/session.go`, `updateObserversLocked`).

### 11.2 `TestReportSaysWhenEveryDivergenceHappened`: 1 in 8 to 10 runs

- **Measured**: once every 8 to 10 runs of `internal/sim`, and it did **not** reproduce in 8
  consecutive runs of the test alone nor in 6 of the whole package. It was written down in
  `SESSION-JOURNAL.md §37` as "not investigated", with the hypothesis "it looks like clock under
  machine load".
- **The hypothesis was right and the reason is more precise than "load"**: the assertion
  that failed was *"the player connection's extras are interleaved"*, and that needs **more
  of our messages before the window than the original had**. Our server emits one ack per
  simulation tick and the fixture's original recorded **three** for that second, so the
  assertion needs at least **five of our eight 125 ms ticks to land inside the first
  wall-clock second**. That is how many ticks the host's scheduler managed, which is not a
  property of the protocol and not something a test may assert. It is also not near a
  boundary that a tolerance could absorb: it is a count of ticks.
- **What changed**: the assertion is replaced by a **stronger deterministic** one. The
  observer connection is required to be settle-only — structural, since its whole handshake
  is stamped at the instant the window opens — and for **both** connections the verdict
  word, the two counts in the verdict sentence, the counts in the totals line and the "the
  split is exact" line must be **four views of the same numbers**. Both conclusions stay
  covered end to end without a replay by `TestCompareConnReportsTheTimes` (interleaved) and
  `TestMessageTailIsNotAMustMatch` (settle), and at the unit level by
  `TestJudgeTailSettlesOnly` and `TestJudgeTailInterleaved`.
- **Falsifications**, both with the defect reintroduced and the tree restored: inverting
  `SettleOnly` gives "the observer connection (port 51002) reached \"interleaved\""; making
  the totals line print one count too few gives "the totals line says 4 of ours before the
  window against the original's 4, which is 0 extras before it, and the split line says 4".
- **Not reproduced, and why that is not a gap**: it was never reproduced on purpose, because
  reproducing a flake by loading the machine is what the previous attempt paid for. It was
  settled by reading the assertion and the arithmetic behind it, and proven with the two
  falsifications above, which fail on the defect rather than on the host.
## 12. MOVED on 2026-10-06: the WebSocket upgrade latency

- **It is no longer here and it is not a bug.** The timing criterion was decided in
  `INTENTIONAL-DIVERGENCES.md`'s header: **a difference in WHEN a client receives
  something, with the bytes and the message sequence identical, is not a
  divergence.** No byte differed here, only how long the client waited, and a client
  that gets the same bytes sooner is a client that works.
- **The decision is the user's**, taken on 2026-10-06 with the reason "the clients work
  the same, it is only the connection and it affects nothing". It closed a gap that had
  been open for this entry's whole life: whether "on the wire" included timing **was not
  answered anywhere in this repository**, and this entry was filed as unintentional
  *only* because no reason was on record, **not** because the criterion had been read to
  include timing.
- **Where the evidence and the measurements went**: `INTENTIONAL-DIVERGENCES.md` 12, with
  every figure intact. **Nothing was thrown away**, and the `-simulationPeriod` sweep, the
  three points on a line that showed ours does not move with the period and the original's
  does, is the substance of what is now a written reason.
- **What is NOT decided by this**: anything where a delay changes the CONTENT of what is
  sent. The criterion clause says so in its own words, and the observer step's period is
  the case that keeps its place: it decides **which frame** an observer is served first.
