# Intentional divergences from the original

> **What this file covers.** Every place where `src_go/` deliberately departs from the
> original, **with the reason the difference is accepted and the evidence that holds it
> up**.
>
> **The criterion, which is the only thing that decides what goes in here.** A divergence
> is a difference **that is visible from outside**: one that a client, a peer, a replay
> server or any consumer of the wire can tell in the bytes or in the message sequence. If
> the only difference is how the code is organised inside —a map against a slot table, a
> pooled buffer against a reserved one, a cheaper algorithm that produces the same
> output— **it is not a divergence** and it is not here: it is in
> [`PERFORMANCE.md`](PERFORMANCE.md) if it is about cost, and in
> [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) if it is about why it was decided. Section 7 lists
> everything that left here for that reason and where it is now, so that it does not come
> back as a finding.
>
> **Criterion, TIMING: decided 2026-10-06.** The criterion above says a divergence is a
> difference visible **in the bytes or in the message sequence**. It did not say what
> happens when **no byte differs** and only the clock does, and one entry hit that gap.
> **The decision: timing is NOT part of this criterion.** A difference in *when* a client
> receives something, with the bytes and the sequence identical, is **not a divergence** and
> does not belong in this register.
>
> **The reason, which is the whole of it**: the consumers of this protocol are the game and
> an observer, and neither reads the wire with a stopwatch. A client that gets the same
> bytes in the same order, sooner, is a client that works. The one thing that made this
> worth deciding rather than assuming is that **"on the wire" was read two ways** and the
> first entry filed under the wider reading (`UNINTENTIONAL-DIVERGENCES.md` 12, the
> WebSocket upgrade latency) was undone by it. `SESSION-JOURNAL.md` §50.
>
> **What this does NOT cover, and the boundary is deliberate**: *observable timing that
> changes the content of what is sent*. If a delay causes a message to land in a different
> second, changes which peer a message names, or changes a count, then the bytes or the
> sequence DO differ and this clause does not apply — that is an ordinary divergence and
> belongs in one of the two registers like any other. The observer step's period is the
> clearest case: it is measured in ticks and it decides **which frame** an observer is
> served first, and that has never been in question.

> **Status.** Current. It is the "we chose this" half of the divergence register; the
> other half, "this is a bug or an unexplained gap", is in
> [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md). **The two
> files are mutually exclusive: no finding appears in both.** If something has no reason
> to accept it, it does not belong in this file.
>
> **Evidence.** Every row cites the binary (`src/sub_*.cpp` with a line) or a capture.
> Where the evidence is a section of the journal it is cited as `SESSION-JOURNAL.md §N` and the
> original text is still there: the journal is the history and it is not deleted.
>
> **Rule.** Rule 1 of `AGENTS.md`: only what is demonstrated. No row here is "I think it
> works the same": either there is a line of the binary, or there is a capture, or it says
> that the line does not exist.

The design decisions with a reason that are **not** divergences (they are things we do
exactly as the original does) live in `SESSION-JOURNAL.md §16.1`. The corrections already made, with
their commit, live in `SESSION-JOURNAL.md §16.3` and in the history of `src_go`.

---

## 1. What the original does and here is not done, with the written reason

### 1.1 The beacon does NOT advertise the WebSocket port (sends 0)

- **What diverges**: the **beacon's WebSocket port field** goes to 0 always, regardless of
  which port the listener starts on. It is one byte of the UDP payload, so anyone listening
  on that port can see it.
- **The nuance that has to be said with the same weight**: the 0 **is also the original's
  default value**, so with the default configuration both sides send the same bytes. The
  difference only shows if an operator passed `-webSocketPort`, and the committed
  captures **do** pass it (`14003`/`15003` in
  `primary_args`/`secondary_args`), but the beacon is not in the harness's comparison:
  it is UDP and it is not captured.
- **Reason**: user's decision, 26/09/2026. The field exists in the beacon payload but the
  port is not advertised on the LAN; a browser/watch client that wants it needs it
  configured from outside.
- **Evidence**: the field is the seventh of the payload, `+0x234`, and the original writes
  it from the value of `-webSocketPort` (`relay.md:117`); here a constant writes it
  (`internal/server/broadcast.go:10`, `broadcastWebSocketPort = 0`). The decision in
  `SESSION-JOURNAL.md §16.1` point 6.
- **Accepted consequence**: a browser/watch client does not find the server by discovery.
  That is the point.

### 1.2 The relay beacon intentionally uses the compatibility broadcast matrix

- **What diverges**: the implementation intentionally broadcasts on two default
  ports (`9999` and `8888`), emits the current `0x21` format together with the
  legacy `0x20` format, uses each interface's broadcast address, and allows
  `-relaybroadcastport 0` to disable the beacon.
- **Why**: this compatibility behavior is intentional for existing LAN
  discovery clients and deployments. It is not a fidelity bug to be resolved
  by changing the server to the original's single-port behavior.
- **Evidence for the original**: the original uses one configurable port whose
  default is `9999` (`main_1400d1310.cpp:216`, `:682-686`), sends only `0x21`
  (`sub_1400D81E0:264`, `:328-332`), targets `255.255.255.255`
  (`sub_1400D6790:339`), and has no `0x20` factory case
  (`sub_1400ED400:1211-1234`).
- **Evidence for this implementation**: the defaults and disable behavior are
  in `internal/config/config.go:98-105`; the per-port formats are built by
  `internal/protocol/messages.go:156-182`; and interface broadcast selection
  is in `internal/net/udp.go:68-72` and `:114-126`.
- **Accepted consequence**: UDP discovery is intentionally broader and is not
  byte-for-byte equivalent to the original beacon. The capture proxy records no
  UDP, so this decision is outside the TCP/WebSocket capture comparison.

### 1.3 The session-setup of the internal `RequestJoin` (`sub_1400EA680`) is not implemented

- **What diverges**: the original, when it handles a **REQUESTJOIN** peer message (the
  type 2 case of the internal switch), looks up or creates the session and registers the
  peer in the global-ids map with `sub_1400E5B00`. Here, when that same message arrives,
  **only the peer's key is recorded** and the session is not touched.
- **Why this IS a divergence and not a note**: its observable consequence is the extra
  second peer that shows up in the bsPort capture, and that consequence is
  **open**, with no written reason to accept it, in
  [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) point 2.
- **Reason**: the path the game really uses is the `LobbyConnect`, which is the one the
  five committed captures validate; the internal setup is reached from the same peer
  message that in the bsPort capture arrived on a connection other than the `REQUESTJOIN`,
  and without the fact of **which of the two messages the original rejected**, implementing
  it would make the known state worse. See point 2 of the other file.
- **Evidence**:
  - `sub_1400DAAF0:193` is the **only** caller of `sub_1400EA680`, and it is the case 2
    (`REQUESTJOIN`) of its switch, whose labels are `2, 3, 5, 6, 7, 9` (`:133-140`).
  - That `switch` is reached from the `case 0x0F` of the dispatcher (`sub_1400DE050:399`) and
    from the recursive dispatch of the `0x13` (`sub_1400DE050:567`), not from a top-level
    `0x01`: **the original's dispatcher has no case for `0x01`** (the ones it has
    are `0x09, 0x0F, 0x10, 0x13, 0x14, 0x17, 0x18/0x1C, 0x1A, 0x1B, 0x1D, 0x1F`, plus the
    `0x15` that is handled before the switch, `sub_1400DE050:136-154`).
  - The game never sends a top-level `0x01`: finding `_7`, no `01...` frame in
    `main_log_7`. `SESSION-JOURNAL.md` §16.4 (row "§3.9b", a row of a **plan** document
    that is not in this repository, so the row is named and not linked) and §3.
  - `sub_1400E5B00` writes the `session+2080` tree and its only caller is
    `sub_1400EA680:259`.

---

## 2. Decisions of the original that are simplified here, and that reach the wire

### 2.1 The first peer's version (`session+2072`) is not read

- **What diverges**: the original decides with the `session+2072` field whether the `0x0A`
  carries the **trailing checksum block**, and here that block **is always sent**. It is a
  block of bytes in the body of the ack, so the difference shows on the wire.
- **Where the field comes from, which is what was not written down**: `peer+68` is the
  **`protocolVersion` that the public client published in its `LobbyConnect`** — the
  `LobbyConnect` writes it into the connection at `sub_1400EB5F0:470` and `:491`
  (`*(_WORD *)(a2 + 68) = *(_WORD *)(a3 + 88)`, and `+88` of the message is the u16 that the
  parser reads in `LobbyConnectMessage__vmethod_20:64-70`), and the connection constructor
  sets it to 0 (`sub_1400D4770:34`). The first tick with a store caches it **only once** and
  only if it is zero (`sub_1400E2590:233-234`), the tick copies it into the message's version
  field (`sub_1400E2590:301`) and the serializer emits the tail **only if it is > 1**
  (`RLink::SessionMessage::MultiGameDataMessage__vmethod_18:146`).
- **Given what was measured, the block comes out the same on both sides**: the
  `LobbyConnect`s of the committed captures announce **5** —read byte by byte from the log,
  offset 17 of the body carries `05 00`: both of `2026-09-30_201122` (one per connection)
  and the one of `2026-09-30_010531`—, that is `> 1` and the block goes out. Fixing it
  **would not change one byte of any capture**.
- **The second reason, which is the one still open**: the **moment** at which the version is
  read is not established. A peer's `LobbyConnect` can arrive after the store exists, and
  the cache is written before that peer exists. The session knowing the join order is no
  longer the reason (that was settled by the 40); the reasons that remain are these two.
- **Evidence**: `sub_1400E2590:233-234`, `:301`, `sub_1400EB5F0:470` and `:491`,
  `LobbyConnectMessage__vmethod_20:64-70`,
  `MultiGameDataMessage__vmethod_18:146`. `SESSION-JOURNAL.md §32.4` point 1 and `§33` ("what is left
  undone").

### 2.2 Reconciliation compares with more peers than the original compares with

- **What diverges**: `sub_1400E4940:114-116` compares the first **participating** record
  against **the next record in the vector**, which need not be participating (the loop that
  finds the first participating one is at `:85`, and the empty-queue cut is at `:74-81`, but
  the second record is not filtered). We compare the front record of **every** active peer
  against the first one's. With three peers and different participations the two paths
  produce different bytes in the checksum tail, so the difference reaches the ack.
- **Reason**: with two peers they agree; with three they only differ when the peers differ
  between themselves, and **no capture has three peers**. It is declared, not blindly fixed.
- **How it would close**: a LAN match with three or more real clients, which is the row
  that already appears in [`PENDING.md`](PENDING.md) and in the matrix of
  [`CAPTURES.md`](CAPTURES.md).
- **Evidence**: `sub_1400E4940:74-81`, `:85`, `:114-116`. `SESSION-JOURNAL.md §33`.

---

## 3. The two that the original does model and that are left unwritten here

### 3.1 `peer+89` and the gate at `E2590:226`: the store is not created until EVERYONE has uploaded

- **What diverges**: the original **does not create the store nor send the first `0x0A`**
  until all the peers have uploaded at least once, because the loop at `E2590:224-231`
  exits as soon as it finds one with `peer+89 == 0` (unless `session+2142 != 0`) and
  skips the store creation. We start with the first one.
- **Reason**: with a single peer it is not noticeable; and the only capture with two clients
  —the bsPort one— has **one** peer in the original (§29) and **two** in ours, so the gate
  would stop the acks of a session the original keeps serving. Implementing it without
  settling the peer divergence first **would make it worse**.
- **Evidence**: `sub_1400E2590:224-231` (the gate), `:250-258` (the creation that is
  skipped), `:42-44` (the `peer+89` that sets it to 1 in `E2E00`). `SESSION-JOURNAL.md §32.4` point 2.

### 3.2 `peer+90` in `E2E00:35`: a peer that joins late has no mark

- **What diverges**: `if (v4 != 0 && (*(_BYTE *)(a1 + 2141) == 0 || *(_BYTE *)(v4 + 90) != 0))`
  — once the store exists (`session+2141 != 0`), a peer that joins later has no mark, and
  its uploads **are silently discarded**. We accept them. What reaches the wire is the
  pending list and the subs of the `0x0A`: without the mark, the peer does not appear in
  the list and its subs are not drained.
- **Reason**: it is the "a player joins a match in progress" case, which **the game does
  not allow** and which no capture has. Same reason as 3.1.
- **Evidence**: `sub_1400E2E00:35`; the mark is set by `sub_1400E2590:230-232`, which is on
  the path that is no longer walked for a peer that arrives late. `SESSION-JOURNAL.md §32.4` point 3.

---

## 4. Divergences whose non-implementation is the reason not to take the risk

### 4.1 The cap of 17 chunks per tick

- **What diverges**: the drainer takes **one** chunk per call and the tick calls it **once
  with the expiry check flag and sixteen more times without it**
  (`sub_1400E2590:314-320`), so a tick carries at most 17 chunks and the rest stay queued.
  We have no such cap: the drainer keeps going while there is queue.
- **Reason**: it is the most clearly demonstrated thing left, and it is **not** implemented
  because it changes **how many `0x0A`s go out per second**, which is exactly what
  `[tick-cadence]` and `[message-count]` have as informative and what we already know we
  group differently. Also the frame store keeps the whole `0x0A`, so changing the grouping
  **changes the content of the replay**. And there is no capture that separates "17
  chunks" from "all of them": with a client that uploads one chunk per tick —which is what
  the five captures are— both behaviours give the same bytes.
- **What the drainer bounds on its own**, and which is not this cap: each chunk keeps the
  message body under `< 0xF7FF` bytes and the sub-message list under `< 0xFF` entries
  (`sub_1400E34F0:152` and `:155`, and the tail block with the same pair of guards
  at `:356`).
- **Evidence**: `sub_1400E2590:314-320` (the sixteen calls) and `sub_1400E34F0:124-231`
  (the copy of one chunk). `SESSION-JOURNAL.md §33` ("what is left undone").

## 6. What is wire-visible and accepted, and why

These read as "different" and they **are** wire-visible differences: a client or the
harness can count them. What they are not is **bugs**, and each one carries the reason it is
accepted. Writing them here is the only way of keeping them from showing up as a finding in
the unintentional file, and saying so is what keeps this file honest: the difference between
"accepted" and "not a divergence at all" is that the first has a reason and the second does
not need one.

- **Grouping of the `0x0A`s**: 3447 acks with subs against 3470, and 3230 checksum blocks
  against 3118, for the same 12312 subs and the same 4948 entries. The `0x0A`s are
  built **per simulation tick**, so a compressed replay groups differently without being
  wrong. The **number** of frames is not must-match; the **order** and the **starting
  point** are. `SESSION-JOURNAL.md §25` and `§28`.
- **The 4 extra acks of second 26**: **they are not ours**. In the run the capture itself
  recorded, the original put 3 in that second and ours 2, with 8 and 8 in each of the 62
  following and the same 500 in total. The mechanism is that the capture's clock is whole
  seconds and the replay feeds each message at its stamp. Closed in
  `SESSION-JOURNAL.md §28`; the `tick-cadence` rule writes it into the reports.
- **The probe (probe-phase)**: the original accepts it as WebSocket
  (`WebSocketManager::SockAccept`) but its 1.3 s animation loop is too slow and the game
  closes the probe at t+200 ms, so it **never answers**. We answer instantly. Measured in
  `battle-server.txt`: `Socks::AnimateAll` exits **78** times, of which
  **70** are `Wait for IO complete; timeoutMS=1300` (the other 8 are `sendwait` and
  `shutdown request`). `SESSION-JOURNAL.md §6`.
- **The replay socket is never opened.** The original built the uploader and opened a TCP
  connection to `-replayHost:-replayPort` at start-up, but **the original does not actually
  upload**: the user ran it with a listener on the replay host, with `-uploadVia=lobby`, and
  **the TCP connection arrived and no request ever followed** — not with a short run, and not
  with a match played to the end. `battle-server.txt:18` shows the uploader being built
  (`instantiating WorldwideReplayUpload @ 127.0.0.1:8443`) and `:23` the connect being
  *deferred and never completed*, with no request in the remaining 423 lines. **Reason**: the
  only artefact the original produced there was a connect that carried no byte, so there is
  nothing on that wire to be faithful to, and this implementation does not upload at all. The
  replay data is still produced and handed to the embedding host through
  `ReplayDataHandler`, and the observer frame store is untouched. The original's seven replay
  flags stay registered, are accepted with any value and are ignored. **Evidence**: the
  measurement above, `battle-server.txt:18,23`, `SESSION-JOURNAL.md` §52, and
  `PLAN-replay-upload-removal.md`.

---

## 12. The WebSocket upgrade is answered on the connection, not on the next tick

- **What diverges**: the time a client waits between connecting and receiving its `101`
  response. The original defers the upgrade to its next main-loop tick, so its latency is
  "wait for the tick, plus the work"; ours is served on the connection's read path and waits
  for nothing. **No byte differs and no message sequence differs.** That is what put it in
  the other file, and the criterion clause in this file's header is what keeps it here.
- **Measured medians on this machine, `windows/amd64`**:

  | `-simulationPeriod` | ours | original |
  |---|--:|--:|
  | 30 ms | **1,12 ms** | 36,9 ms |
  | **125 ms** (the default) | **1,12 ms** | **128,2 ms** |
  | 500 ms | **1,09 ms** | 513,3 ms |

  **Ours does not move when the period moves, and the original's does.** That is the whole
  finding, and it is three points on a line rather than one comparison.
- **The reason this is accepted, written down**: a consumer of this protocol is the game or an
  observer, and **neither reads the wire with a stopwatch**. The bytes and the sequence are
  identical; what differs is that we are faster. Reproducing the deferral would **make every
  client wait longer for no observable gain**, and it would delay every observer's first frame
  by up to a full period.
- **The distribution, because the medians alone would be a bad summary**: the original's own 15
  samples are two clusters with a gap and nothing in it —
  `0.54 0.95 1.14 1.97 3.14 · 124.90 127.60 128.16 128.19 131.34 132.75 133.88 135.50 135.52
  135.84` — so the extra time is a whole number of periods and not a slow path. Ours is `0.55
  0.56 0.57 0.58 0.61 1.08 1.12 1.12 1.14 1.14 1.77 1.98 2.63 5.38 15.07`: no cluster at any
  period. **How often each cluster occurs is not reproducible run to run**; a later run gave
  15 of 15 in the original's slow one. The medians are stable, the frequencies are not, and
  that is why the tool prints the samples.
- **The floor under every figure here**: this machine's monotonic clock reads **0 for 94 of 200**
  loopback TCP round trips, median 509 µs over the rest. Our figures are only about one order
  above that floor, so "1,12 ms" is a usable number and not a precise one, and the tool counts
  a sample it could not resolve rather than carrying it in as a zero. The original's 125 ms is
  two and a half orders above it, and the claim rests on the slow cluster and the period sweep,
  not on the fast samples.
- **Why it was not noticed for as long as it was**: `PERFORMANCE.md` §10 had the row and called
  it **"not comparable"**, blaming a first-connection cold path on our side. **Both halves were
  wrong.** It was isolated on 2026-10-06, and the cold path is not ours: what made the row
  unreadable was a **defect in the tool**, `benchpeer` taking **one** upgrade sample per
  repetition and reporting the median of a bimodal distribution, so which number landed in the
  table depended on the tick phase and on N. At N=5 the row could invert. The tool now takes
  `-upgrades N` samples per repetition.
- **One thing it does NOT affect**: the harness. The settle is `interval+2` ticks and the
  original's deferral is at most one period, so the `upgrade-response` MUST-MATCH rule is already
  covered and `internal/sim` needs no change.
- **Evidence**: `PERFORMANCE.md` §10 (the table, the distribution and the period sweep),
  `SESSION-JOURNAL.md` §49 and §50, and the tool that took the figures,
  `src_go/cmd/benchpeer`. On our side the path is `internal/server/ws_listener.go` (the upgrade
  runs on the connection's read loop) against `internal/server/server.go` (the 125 ms tick).
  **No capture contains a timing that separates them** — the proxy stamps whole seconds, which
  is why this took an external measurement and not one of the `CAPTURES.md` rows.

## 7. What was retired from this file, and where it is

Every row is something that was here and is **not an observable difference**: either because
it is only an internal decision, or because the claim was false and what is there now is
the correction. The "what is visible" column says which of the two it is, and the "where it
is" column says where it is now held to account.

| what was there | what is visible from outside | where it is |
|---|---|---|
| The `0x08 MultiGameData` is not sent to the observer | **no, and the claim was false**: `sub_1400E2590:360` builds the message with `MultiGameDataMessage__vmethod_18`, whose wire type is **10 (`0x0A`)** and whose recipients are **the peers** (`:362-371` walks the slot vector and sends to `a1[peerID+3]`), not the observers. In `src/` **there is no type-8 class at all**: we do send the `0x0A`s, and the ones the original sends go to the peers | nothing to relocate: there was no divergence. The observer is served by `sub_1400E4EC0` from the store (`E2590:96-97`), which is what we do |
| The `DropoutPeer` gate (`*(u16*)(peer+68) < 3`) is assumed to hold | **no**: the `if` at `sub_1400DAAF0:141` requires `v63 == 8`, and the switch at `:133-140` only has the labels `2, 3, 5, 6, 7, 9`. It is dead code, so the original **also** forwards everything. Also the field was not what was claimed: `peer+68` is the `protocolVersion` of the `LobbyConnect` (`EB5F0:470`/`:491`), not a transition counter | `SESSION-JOURNAL.md §16.4`; the DROPOUT filter exists on neither side |
| A native peer message is not re-serialised: the `0x0F` goes verbatim | **no**: the "host only" verdict **has no producer** (the same unsatisfiable `if`), so the original only forwards and so do we. We agree | `SESSION-JOURNAL.md §16.4`; the verbatim forwarding is in `internal/server/handlers.go` (`handlePeer`) |
| The relay has no third "host only" output | **no**, for the same reason: `sub_1400DF920:136-153` maps `1` -> everyone, `2` -> host only, and nobody produces the `2` | `SESSION-JOURNAL.md §16.3` and `§16.4` |
| The ack/retransmission semantics of `PEERHEADER` | **no**: there is no path that emits a native peer message, so there is nothing to retransmit or to ack. The zlib of the `0x0F` stays un-inflated for the same reason (the original only inflates in `vmethod_28`, which is the trace path) | `SESSION-JOURNAL.md §16.4` |
| The endianness of the serializers that are not sent | **no**: no byte goes out. "Fixing them" without verifying would be inventing, and there is nothing on the wire that could be wrong | `SESSION-JOURNAL.md §16.4` (row D.10) |
| `maxreplay` still has no identified consumer | **no**: it is a JSON key with no consumer found, not a behaviour difference. Also the citation that came with it was retired: `sub_1400D8E10:1145-1146` decrements a counter in the TLS's **filename table** (`strrchr("D:\\a\\1\\s\\Server.cpp", 92)` at `:1112`), not of `maxClientDosAttackCount` | [`PENDING.md`](PENDING.md) 2.4, which is where a "has to be read" lives. The two parsers that read the three keys together: `sub_1400D3570` and `sub_1400D36E0` |
| The `titleID` is accepted even if it does not match | **no, and the claim was false**: the lookup **does** use the pair. `sessionManager.GetSession(SessionKey{TitleID, SessionID})` with 16-bit and 64-bit `titleID` and `sessionID`, which is what the original's tree compares (`sub_1400EC060:58-77`, and the same comparator at `sub_1400EA680:65-85`), and the legacy shape puts `0xFFFF` just like the original's parser (`StartObserveRequestMessage__vmethod_20:63`) | nothing to relocate. Where the detail is, is `internal/server/handlers.go` (`handleStartObserve`) |
| `Session.Peers` is still a `map[byte]*Peer` | **no**, and **it is also false**: `Peers` is `peerSlots`, a `[256]*Peer` indexed by player slot (`internal/session/session.go`, `type peerSlots [256]*Peer`), which is the original's own shape (`sub_1400E5670:100` writes `session + 24 + 8*peerID`), and **every** iteration goes through `peerOrder`, which is the join order | [`PERFORMANCE.md`](PERFORMANCE.md) 7, with the before/after measurement of `SESSION-JOURNAL.md §40.2` |
| The ack body is not pooled, and everything else that is not pooled | **no**: they are allocations, and the same buffer ends up in two messages if it is pooled. It is a property, not a preference | [`PERFORMANCE.md`](PERFORMANCE.md) 5 and 7 |
| When a connection dies, here its observer-list entry is removed | **no, and the claim was false**: the original **does** remove it, at `sub_1400E52D0:227-242` (it looks for the entry whose connection is the one going away, `memmove` and `-16`). The end state agrees; the only thing we do not do is empty the list beforehand | `SESSION-JOURNAL.md §31` and `§36.1`; the ordering after releasing the lock is written in `internal/session/session.go` (`tickLocked`) |
| The observer list is emptied when it is cleared | **no**: it is memory. What goes on the wire —the `0x19` = 4 to each observer and the close— is done the same way, and the entry goes the same way when the connection drops | `SESSION-JOURNAL.md §36.1` and `§32.5`, and the code (`tickLocked`) |
| `Session.Tick` does not return the two status bytes of `E2590:483` | **no**: only the munin reads them (`sub_1400E9E90:69-71`, `HIBYTE(v41)` accumulated and `LOBYTE` tested), which does not travel. The high byte also **can never be set**: `E2590:285-286` writes it only if `v28 == 0`, and the only `++` of the field is at `:380`, after | `SESSION-JOURNAL.md §36.3` |
| The observer step: whether the original serves the freshly activated one in the same tick | **closed on 2026-10-05 by reading the binary**: it does **not**, and there is no arm that could, because `sub_1400E43E0` builds exactly one message (the `0x1E`) and never calls the two functions that fetch frames from the store. What the step costs is `interval+1` ticks, which `internal/session`'s `TestTheWorstPhaseCostsTheWholePeriod` measures | nothing to relocate. The record is [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 6 and [`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) §44.4 |
| The `0x13` does not feed the replay uploader (§4.2) | **no**: the uploader no longer exists, so neither `0x10` nor `0x13` builds a POST | this file §6, "The replay socket is never opened" |
| An upload without a session is discarded (§5) | **no**: there is no upload to discard | this file §6, "The replay socket is never opened" |
| The byte range of the replay upload (a §6 bullet) | **no**: no request is sent, so there is no body to range over | this file §6, "The replay socket is never opened" |

---

## 8. Strings from the original that stay, and why

`DERIVATIVE-WORK-RISK.md` found six log strings in `src_go` copied verbatim out of the
original binary, and they were rewritten in this project's own wording
(`SESSION-JOURNAL.md` §47.4). Searching every string literal of 12 characters or more
against `src/**/*.cpp` leaves the rows below matching, and **each one stays**.

They are here because the criterion in §0 is *observable from outside*, and these are:
the first group is what a **client** reads off the wire or out of the handshake, and the
second is what the **operator** types. Renaming any of them would break interoperability
or the documented command line, and none of them is a developer diagnostic.

| what stays in `src_go` | who reads it | why it cannot be changed |
|---|---|---|
| `WebSocket++/0.8.2` | the client's handshake parser | it is a `Server:` header value on the wire, measured on both WebSocket captures; the harness compares the header set of the two answers so a difference cannot come back unnoticed |
| `M#%08u Sack%08u NSack%08u` | nobody at run time | it is quoted only to say that two of the original's own diagnostics disagree about one header field's name and agree on its offset, so only the offset is treated as confirmed. Renaming the quoted form would make the claim unverifiable |
| `Sec-WebSocket-Accept`, `Sec-WebSocket-Key` | the client's handshake | RFC 6455 header names |
| `RSA PRIVATE KEY` | the PEM parser | the PEM label |
| `-maxPagesToStorePerSession` | the operator | the upstream flag name, registered as an **alias** so a launcher that passes it is not rejected. `internal/config/config.go` |
| `-simulationPeriod`, `-observerUpdateIntervalMS`, `-observerSocketSkipMS`, `-logMod`, `-logModLevel` | the operator | the upstream flag names, for the same reason |
| `255.255.255.255` | the relay beacon | the broadcast address the beacon goes to |

**What was removed, for contrast**, and each of these *was* a developer diagnostic with no
client reading it: the unknown-message warning, the observer-kick notice, the
store-refusal notice, the sync-timeout reason, and two ordering-case messages. The sync
reason is now a named constant, `internal/session.syncDataTimeoutReason`, because it is the
only one of the six that a test asserts on.
