# BattleServer.exe — the binary, and the Go reimplementation of it

This repository holds two things and a set of notes about them:

- **`src/`** — the **ground truth**: 7,450 decompiled C++ files from Microsoft's
  `BattleServer.exe` (Age of Empires II: Definitive Edition / Age of Empires III:
  Definitive Edition), one per function, with the address and the caller list in the
  header. Nothing in `src/` is modified by anything in this repository. Every claim about
  what the original does is cited back to a file and a line number in here.
- **`src_go/`** — a **byte-for-byte reimplementation** of that server in Go, a Go submodule
  with its own commit history. It is verified against the original, not against a
  description of it. Its comments state the wire contract and the behaviour; they do not
  cite the decompiler, and what that means for distribution is in
  [`PROVENANCE.md`](PROVENANCE.md).
- **the documents in this directory** — what was learned from `src/`, kept separate by
  subject.

**If you are here for the first time, read [`AGENTS.md`](AGENTS.md).** It is the front door:
the rules, how the verification harness works, how to deploy, and which document answers
which question. This file is only an index, and it does not repeat anything another document
already says better.

---

## The documents

### The living set, by subject

These are current and each one owns its subject. Two of them are **mutually exclusive**
registers of the differences between the original and the reimplementation, and the
criterion that decides which register an entry belongs to is stated at the top of each.

| document | what it is about |
|---|---|
| [`AGENTS.md`](AGENTS.md) | **the front door**: rules, the harness, deployment, and this index's entries in full |
| [`bsPort.md`](bsPort.md) | the game port: its flags, the RLink framing, 27012 |
| [`webSocketPort.md`](webSocketPort.md) | the WebSocket port: **RFC 6455** (not Hixie-76), and the handshake |
| [`oob.md`](oob.md) | the out-of-band port |
| [`relay.md`](relay.md) | **the presence beacon** — the UDP announcement, `-relayBroadcastPort` |
| [`LOBBY-PROTOCOL.md`](LOBBY-PROTOCOL.md) | the heartbeat with the lobby |
| [`MESSAGES.md`](MESSAGES.md) | the table of message types, and where each number comes from |
| [`replays.md`](replays.md) | the original's replay pipeline and HTTP upload (this server does not upload) |
| [`uploadVia-lobby.md`](uploadVia-lobby.md) | the original's `-uploadVia=lobby`; the flags are accepted and ignored here |
| [`uploadVia-lobby-connection.md`](uploadVia-lobby-connection.md) | the original's "connect deferred" diagnosis |
| [`uploadVia-lobby-state.md`](uploadVia-lobby-state.md) | the original's connection states and upload conditions |
| [`INTENTIONAL-DIVERGENCES.md`](INTENTIONAL-DIVERGENCES.md) | differences we **chose**, each with its reason and its evidence |
| [`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) | the open bugs, the unexplained gaps, and the questions with no answer |
| [`PENDING.md`](PENDING.md) | what is missing, split by **who can resolve it** |
| [`CLOSED-QUESTIONS.md`](CLOSED-QUESTIONS.md) | the two questions that are closed, and the evidence that closed them |
| [`PERFORMANCE.md`](PERFORMANCE.md) | CPU, RAM, allocations, goroutines, and what was left untouched |
| [`CAPTURES.md`](CAPTURES.md) | how to record a capture, the matrix, the results, and **the ceiling of what the harness can ask** |
| [`PROVENANCE.md`](PROVENANCE.md) | **what this repository is, and what may not be redistributed**: the per-path list, and the statement that this is not a clean-room implementation |
| [`BEHAVIOUR-NOTES.md`](BEHAVIOUR-NOTES.md) | the recovered protocol structure **written as behaviour** — the knowledge the Go comments deliberately no longer carry |

### One superseded, kept

| document | why it is still here |
|---|---|
| [`WEBSOCKET-BSPORT-COMPARISON.md`](WEBSOCKET-BSPORT-COMPARISON.md) | its **conclusion** is superseded (on bsPort you cannot observe, and the TLS belongs to the port), but its packaging, handshake and socket-manager detail still holds. It carries a banner saying so |
| [`DERIVATIVE-WORK-RISK.md`](DERIVATIVE-WORK-RISK.md) | the 2026-10-06 source review that asked for the `src_go` citation removal and for [`PROVENANCE.md`](PROVENANCE.md). Its recommendations were carried out, so read it as **history**, not as a description of the tree as it now stands |

### The history

[`SESSION-JOURNAL.md`](SESSION-JOURNAL.md) is ~7,100 lines of rounds, with the measurements
and the falsifications where they were produced. It carries an index at the top mapping
section → topic → living document, so the index alone is usually enough to find a fact.
**Read it last, and by sections.**

[`go1.27.md`](go1.27.md) is a copy of the Go 1.27 release notes. It is not a project
document; `SESSION-JOURNAL.md` cites it.

### Three documents that used to be here and are not

- **Replay upload via MySQL, and via an ODBC driver.** Both are gone. The three
  `uploadVia-lobby*` documents and `replays.md` describe the original's pipeline; **this
  server does not upload replays at all**. The original's replay flags are still
  registered, are accepted with any value and are ignored, and the `mysql`/`odbc`
  documents were never recoverable enough to keep.
- **The proxy-capture validation walkthrough.** Gone. It described a capture layout
  (`main_log_*.csv` with a `data_b64` column) that the proxy stopped writing; the layout
  today is in [`CAPTURES.md`](CAPTURES.md) and the validation is the `internal/sim` harness.
  Its measurements were not lost with it — they are in the journal
  (`SESSION-JOURNAL.md:308-312`, the `_4` run: the game died at 294.2 s with **both**
  servers, relays 202/202, ping echoes 71/71).

---

## The ports, and which document owns each

| port | transport | what it carries | document |
|---|---|---|---|
| **bsPort** | TCP, 27012 | game connections, RLink framing | [`bsPort.md`](bsPort.md) |
| **webSocketPort** | TCP + TLS | browser/watch clients, RFC 6455 | [`webSocketPort.md`](webSocketPort.md) |
| **outOfBandPort** | TCP | the latency ping | [`oob.md`](oob.md) |
| **relayBroadcastPort** | **UDP** | the presence beacon | [`relay.md`](relay.md) |
| **lobbyPort** | TCP, 8443 | the heartbeat with the lobby | [`LOBBY-PROTOCOL.md`](LOBBY-PROTOCOL.md) |
| replay upload (not implemented; original only) | HTTP POST, **plain** | the replay body | [`replays.md`](replays.md) |

Two of these are easy to confuse and are repeatedly confused in the notes:

- **The beacon is not the lobby.** The beacon is one-way UDP to the LAN, so a client can
  discover the server. The lobby connection is a TCP conversation the server makes to
  register itself and stay alive.
- **`uploadVia=lobby` is not the lobby either.** "lobby" there is a historical name for the
  original's worldwide HTTP method, and it has nothing to do with `lobbyHost:lobbyPort`.
  The endpoint was `POST /game/replay/setReplayData`; this server no longer connects to it.

### The beacon, in one paragraph

The server announces itself on the LAN so a client can find it without an address. It is
**original behaviour**, built by the original's `Server` constructor and sent by its process
loop; [`relay.md`](relay.md) cites every line. The original sends **one** datagram every
**10 000 ms** to **`255.255.255.255`** on **one** port, **`9999`** by default, carrying the
message type **`0x21`** and the fields *guid, server name, public port, WebSocket port,
out-of-band port* — all little-endian, because RLink writes the raw bytes without
`htons`. Our side differs in three ways that are on the wire and open: it uses **two**
default ports (`9999` and `8888`), it sends to **each interface's** broadcast address instead
of only `255.255.255.255`, and it emits a **legacy `0x20`** message that **the original has
no code for**. Those are
[`UNINTENTIONAL-DIVERGENCES.md`](UNINTENTIONAL-DIVERGENCES.md) 13. The beacon is UDP and the
harness records no UDP, so none of it is covered by a capture.

---

## Command line

**The authoritative list is the code**: `src_go/internal/config/config.go`, which is what
`cmd/battleserver` runs. The defaults below are the original's, read out of the parser's
initialiser block in `src/main_1400d1310.cpp` — the defaults are *not* string literals
anywhere, they are constants in that block.

| flag | default | evidence |
|---|---|---|
| `-name` | `"My Server"` | `main_1400d1310.cpp:336` |
| `-bsPort` | `27012` | `:215` (`v95 = 27012`) |
| `-webSocketPort` | `0` | `:295` (`v97 = 0`) — **always 0 in this project** |
| `-outOfBandPort` | `27212` | `:216` (`LOWORD` of `0x270F6A4C`) |
| `-relayBroadcastPort` | `9999` | `:216` (`HIWORD` of `0x270F6A4C`) |
| `-lobbyHost` | `"localhost"` | `:280` |
| `-lobbyPort` | `8443` | `:283` |
| `-publicPort` | `27012` | `:294` |
| `-simulationPeriod` | `125000` | `:344` (microseconds — 125 ms) |
| `-pageSize` | `65536` | `:347` (`v93 = 0x10000`) |
| `-observerMax` | `512` | `:296` |
| `-region` | `"global"` | `:307` |
| `-muninReportDelay` | `60000` | `:335` |
| `-sslCert`, `-sslKey` | — | paths; the pair is only read for the WebSocket listener |
| `-uploadVia` | — | accepted with any value and ignored: the upload is not implemented |
| `-?`, `-h`, `-help` | — | prints usage and exits (`main_1400d1310.cpp:689-698`) |

Three things that are true of the original and are worth knowing before reading the code:

- **`-webSocketPort` defaults to 0**, and 0 means the WebSocket listener is not built at all
  (`sub_1400D6790:291-300`). Rule 3 of [`AGENTS.md`](AGENTS.md) holds it at 0 in every test
  for that reason.
- **`-uploadVia` accepts any value and does nothing.** The original's replay upload is not
  implemented here, so `mysql`, `odbc`, `lobby` and anything else all start the server. The
  flags stay registered because an original launcher passes them.
- **The help text is four lines** (`main_1400d1310.cpp:691-697`): name, simulation period,
  bsPort, relayBroadcastPort. The other flags are undocumented in the original and are
  documented here or in their own document.

---

## Also in the binary, with nowhere else to put it

These are real and cited, but they are not part of the protocol work and no document owns
them, so they live here rather than being invented into a protocol document.

**Cryptography that is present in the binary:**

| what | evidence |
|---|---|
| RC4 — key scheduling and PRGA, an S-box variant | the OpenSSL engine test string `(TEST_ENG_OPENSSL_RC4) test_init_key() called` at `src/sub_14011D820_14011d820.cpp:20` |
| AES — `pshufb`-based rounds | `src/sub_140008C80_140008c80.cpp:54,60` |
| MD5 — **as an OpenSSL EVP entry only** | `"ssl3-md5"` registered at `src/sub_14011B6C0_14011b6c0.cpp:39`. **No use of MD5 in the server's own code has been demonstrated**, and an earlier version of this file claimed one; the claim is withdrawn |
| hardware RNG, `rdrand` | `src/sub_1400013F0_1400013f0.cpp:22` |

MD5 is worth the negative: the WebSocket handshake uses **SHA-1** for
`Sec-WebSocket-Accept` (RFC 6455), not MD5. See [`webSocketPort.md`](webSocketPort.md).

**Metrics.** `-enableStats` turns on a Munin report written to `-muninLog` under the prefix
`bsmunin` (`main_1400d1310.cpp:817`), every `-muninReportDelay` ms. All nineteen counter
names are registered in one function, `src/sub_1400809C0_1400809c0.cpp`:

`PacketsInUse`, `Clients`, `ValidClients`, `ServerCpuUsage`, `SslAcceptCpuUsage`,
`OobServiceCpuUsage`, `HttpServiceCpuUsage`, `BytesReceived`, `BytesSent`, `DOSDetected`,
`RemovePeerRequests`, `TcpConnections`, `TcpConnectionErrors`, `WsConnections`,
`WsConnectionErrors`, `PingConnections`, `PingConnectionErrors`, `AverageFrameTimeUs`.

---

## The dataset

| path | what it is |
|---|---|
| `src/` | the decompilation: **7,450** `.cpp` files, one per function |
| `headers.hpp` | the class declarations and DLL imports, included by all of `src/` |
| `metadata.json` | `binary_info`, `imports`, `strings`, `functions` — the index used to navigate |
| `BattleServerOrig.exe.pristine` | the untouched original, kept for the identity comparison |
| `battle-server.txt` | the console output of the **original**, 2026-08-11 — the only runtime measurement of its animation loop |
| `src_go/` | the reimplementation (Go submodule) |
| `captures/` | recordings, one directory per run. **Ignored by git** |

### Reading `src/`

The file name is the function's identity:

- `sub_1400E5670_1400e5670.cpp` — an unnamed function at `0x1400E5670`
- `main_1400d1310.cpp` — `main`
- `RLink__SessionMessage__RelayBroadcastMessage__vmethod_18_1400e06d0.cpp` — a vtable method,
  class and method named by the decompiler
- `WWBS__ReplayUploadManager__vmethod_XX_*.cpp` — the same, in the `WWBS` namespace

Every file opens with a header giving the address and **the list of its callers**, which is
usually the fastest way in: the caller tells you the subject and the address tells you the
layout.

`metadata.json` is the other way in — it has the strings, the imports and the function list
in one place, which is better than searching 7,450 files when you only have a literal.

---

## Two things this file deliberately does not say

- **Nothing about whether the original is correct.** It is what it is; the notes describe it,
  they do not grade it.
- **Nothing that another document states better.** The protocol detail is in the protocol
  documents, the capture procedure is in [`CAPTURES.md`](CAPTURES.md), the divergence
  registers are in the two divergence files, and the beacon is in [`relay.md`](relay.md). If
  you find a claim here that one of those also makes, the other one is the one to believe,
  because it carries the evidence.