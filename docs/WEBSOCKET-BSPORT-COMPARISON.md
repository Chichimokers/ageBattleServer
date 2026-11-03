# Comparison: bsPort vs webSocketPort

> **SUPERSEDED IN ITS CONCLUSION, BUT THE REST STILL HOLDS.** This document does not mention
> the **observer** in any of its 394 lines, and there are two measured facts that change that
> from the root:
>
> 1. **On bsPort you cannot observe.** The user's fact. The original stays at **a single
>    peer** on bsPort because there is no observer possible there, not because it counts
>    differently. The conclusion "the two ports are equivalent" cannot be true.
> 2. **The TLS is the port's, not the run's.** The bs listener is built **without** the
>    certificate pair (`src_go/internal/server/server.go:195`); the one that receives it is the
>    WebSocket one (`:199`). The table above ("Encryption: No / Yes") is right, but the implication
>    that was drawn from it, that the two ports can be swapped for each other, is not.
>
> **Evidence**: `SESSION-JOURNAL.md §27` (the two transports and how the harness reads them) and `SESSION-JOURNAL.md
> §29` (why we add a second peer on bsPort and the original does not). And the bug that
> came out of here: the harness decided the TLS **once for the whole replay**, so a
> bsPort capture dialled against a listener that does not speak TLS and ended up with
> `ours=0` against 24 and 2 messages of the original, **with a clean report** (`SESSION-JOURNAL.md §28`).
>
> **What still holds** and does not need touching: the packaging, the handshake, the socket
> managers, and the framing comparison. That is confirmed by the captures and by `src/`.
>
> **Why this file is still here**: its conclusion is superseded, but its **detail still holds**
> and there is no other document that carries it — the packaging, the handshake, the socket
> managers and the framing comparison are confirmed by the captures and by `src/`.
> [README.md](README.md) lists it as the superseded-but-kept one, and
> [webSocketPort.md](webSocketPort.md) links it as a "detailed comparison".
> Of the two options §36.4 recommends —"mark the conclusion as superseded with a
> link", or delete— the first is applied.

## General Description

BattleServer.exe handles **two TCP ports** for game connections:
- **bsPort**: direct TCP connections (native client)
- **webSocketPort**: WebSocket connections with TLS (web/browser client)

Both ports can carry **the same kind of game data**, but with different packaging and encryption.

---

## Quick Comparison

| Characteristic | bsPort | webSocketPort |
|----------------|--------|---------------|
| **Base protocol** | TCP | TCP + TLS |
| **Encryption** | No | Yes (SSL/TLS) |
| **Format** | Direct binary | WebSocket RFC 6455 |
| **Frame wrapper** | None | 0x82 [len] [payload] |
| **SocketManager** | RawSocketManager | WebSocketManager |
| **Handshake** | None | TLS + RFC 6455 |
| **Typical client** | Native application | Web browser |
| **Source** | `sock.cpp` | `WebSocketManager.cpp` |

---

## Similarities

### 1. Same Application Protocol

Both ports carry **the same binary RLink messages**:

| Type ID | Message | Available on both |
|---------|---------|---------------------|
| `0x09` | `GameDataMessage` | Yes |
| `0x10` | `ProxyMessage` | Yes |
| `0x17` | `GameDataAndChecksumMessage` | Yes |
| `0x18` | `StartObserveRequestMessage` | Yes |
| `0x1A` | `ActivateObserverMessage` | Yes |
| `0x1B` | `StopObserveMessage` | Yes |
| `0x1D` | `SetObservableParametersMessage` | Yes |
| `0x1F` | `LeaveSimulationMessage` | Yes |
| `0x13` | `LobbyRelayMessage` | Yes |
| `0x14` | `LobbyPingMessage` | Yes |
| `0x15` | `LobbyConnectMessage` | Yes |

### 2. Same Session Management

Both ports share the same session infrastructure:

- **SessionManager**: one for both ports
- **Peer management**: the same peer system
- **Replay upload**: both generate the same replay data (this describes the original; this
  server does not upload and delivers the data through an in-process callback)
- **Metrics**: they share global counters

### 3. Same Packet Processing

Both go through the same processing pipeline:

```
bsPort:
  Socket → RecvSome() → RLink::Packet → ProcessPacket()

webSocketPort:
  Socket → TLS Decrypt → WebSocket Frame → RLink::Packet → ProcessPacket()
```

The final entry point is the same: `Server::ProcessPacket` (FUN_1400de050).

### 4. Same Shared Sessions

A peer connected over bsPort and another over webSocketPort can be in the **same game session**:

```
Game Session (Session)
  ├── Peer 1: connected via bsPort (direct TCP)
  ├── Peer 2: connected via webSocketPort (WebSocket)
  └── Peer 3: connected via bsPort (direct TCP)
```

---

## Differences

### 1. Data Packaging

#### bsPort (direct TCP)

```
[TCP Payload] = [RLink::Packet]
```

The binary data is sent directly over TCP with no additional wrapper.

```
Hex stream (bsPort):
09                                      // messageType (GameData)
FE                                      // innerType (0xFE = replay data)
1C 00                                   // dataLength = 28
[28 bytes of binary payload]
```

#### webSocketPort (WebSocket)

```
[TCP Payload] = [TLS Record] = [WebSocket Frame]
WebSocket Frame (RFC 6455) = header + mask + [RLink::Packet]
```

The data is wrapped in an RFC 6455 WebSocket frame.

```
Hex stream (webSocketPort):
82                                      // FIN + binary opcode (RFC 6455)
89                                      // Payload len 9 + MASK bit
00 00 00 00                             // Mask (zero in this game)
09                                      // messageType (GameData)
FE                                      // innerType
1C 00                                   // dataLength = 28
[28 bytes of binary payload]
```

### 2. Encryption

#### bsPort

```
┌──────────────────────────────────────────┐
│  TCP socket                              │
│  ┌────────────────────────────────────┐  │
│  │  RLink::Packet (plaintext)         │  │
│  └────────────────────────────────────┘  │
└──────────────────────────────────────────┘
```

No encryption. The data travels in plaintext.

#### webSocketPort

```
┌──────────────────────────────────────────┐
│  TCP socket                              │
│  ┌────────────────────────────────────┐  │
│  │  TLS Record (encrypted)            │  │
│  │  ┌──────────────────────────────┐  │  │
│  │  │  WebSocket Frame              │  │  │
│  │  │  ┌────────────────────────┐  │  │  │
│  │  │  │  RLink::Packet          │  │  │  │
│  │  │  └────────────────────────┘  │  │  │
│  │  └──────────────────────────────┘  │  │
│  └────────────────────────────────────┘  │
└──────────────────────────────────────────┘
```

Full TLS encryption.

### 3. Connection Handshake

#### bsPort

```
Client                    Server
   │                          │
   │─── SYN ─────────────────→│
   │←── SYN-ACK ──────────────│
   │─── ACK ─────────────────→│
   │                          │
   │═══ Ready for data ══════│  ← Immediately after TCP
```

No additional handshake. The data can flow immediately.

#### webSocketPort

```
Client                    Server
   │                          │
   │═══ TCP 3-way handshake ══│
   │                          │
   │═══ TLS handshake ════════│  (Client Hello, Server Hello, Certificate, Key Exchange)
   │                          │
 │   │─── HTTP GET ────────────→│  (Upgrade: WebSocket, Sec-WebSocket-Key)
   │←── HTTP 101 ─────────────│  (Sec-WebSocket-Accept: SHA-1)
   │                          │
   │═══ Ready for data ══════│  ← Only after both handshakes
```

It requires a full TLS + RFC 6455 handshake.

### 4. Socket Manager

#### bsPort: RawSocketManager

```c
// FUN_14009be00(0, 0) - RawSocketManager
struct RawSocketManager {
    void *vftable;
    void *lock;
    CRITICAL_SECTION cs;
    // ... socket fields
};
```

#### webSocketPort: WebSocketManager

```c
// FUN_14009be00(1, hasSSL) - WebSocketManager
struct WebSocketManager {
    void *vftable;
    void *lock;
    CRITICAL_SECTION cs;
    char serverName[16];
    void *sslContext;        // ← SSL context
    void *listenSocket;
    // ... additional fields
};
```

---

## Comparative Flow Diagram

### bsPort

```
┌───────────────────────────────────────────────────────────────┐
│  Native Client                                                │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  GameDataMessage (binary)                               │  │
│  └─────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────┘
                          │
                          ▼ TCP (unencrypted)
┌───────────────────────────────────────────────────────────────┐
│  bsPort (27012)                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  RawSocketManager::SockAccept()                         │  │
│  │  → Creates a Client                                    │  │
│  │  → Adds to ClientList                                  │  │
│  └─────────────────────────────────────────────────────────┘  │
│                          │                                    │
│                          ▼                                    │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  ProcessSocks()                                         │  │
│  │  → RecvSome()                                           │  │
│  │  → ParseMessage()                                       │  │
│  │  → ProcessPacket()                                      │  │
│  └─────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────┘
```

### webSocketPort

```
┌───────────────────────────────────────────────────────────────┐
│  Browser Client                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  GameDataMessage (binary)                               │  │
│  │  └→ RFC 6455 frame: 0x82 [len] [data]                 │  │
│  │     └→ TLS Record (encrypted)                         │  │
│  └─────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────┘
                          │
                          ▼ TCP + TLS
┌───────────────────────────────────────────────────────────────┐
│  webSocketPort (9090)                                         │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  WebSocketManager::SockAccept()                         │  │
│  │  → TLS handshake                                       │  │
│  │  → RFC 6455 handshake                                  │  │
│  │  → Creates WebSocketInfo                               │  │
│  └─────────────────────────────────────────────────────────┘  │
│                          │                                    │
│                          ▼                                    │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  SockPacketReceive()                                    │  │
│  │  → TLS Decrypt                                         │  │
│  │  → Parse RFC 6455 frame (unmask)                       │  │
│  │  → ParseMessage()                                       │  │
│  │  → ProcessPacket()  ← the SAME point as bsPort         │  │
│  └─────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────┘
```

---

## Shared Session

Both ports share the same SessionManager, allowing mixed peers:

```
┌───────────────────────────────────────────────────────────────┐
│  Session (ID: 12345)                                          │
│                                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  Peer 1 (ID: 0)                                         │  │
│  │  ├── Connected via: bsPort (direct TCP)                 │  │
│  │  ├── Socket: 0x1234                                     │  │
│  │  └── Data: GameDataMessages, ProxyMessages              │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  Peer 2 (ID: 1)                                         │  │
│  │  ├── Connected via: webSocketPort (WebSocket)           │  │
│  │  ├── Socket: 0x5678 (TLS)                               │  │
│  │  └── Data: GameDataMessages, ProxyMessages  ← the SAME  │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │  Peer 3 (ID: 2)                                         │  │
│  │  ├── Connected via: bsPort (direct TCP)                 │  │
│  │  ├── Socket: 0x9ABC                                     │  │
│  │  └── Data: GameDataMessages, ProxyMessages              │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ReplayUploadManager: captures data from all the peers        │
└───────────────────────────────────────────────────────────────┘
```

---

## Metrics Comparison

| Metric | bsPort | webSocketPort |
|--------|--------|---------------|
| `TcpConnections` | Connections counted | — |
| `TcpConnectionErrors` | Errors counted | — |
| `WsConnections` | — | Connections counted |
| `WsConnectionErrors` | — | Errors counted |
| `SslAcceptCpuUsage` | — | Handshake SSL CPU |
| `BytesReceived` | (shared) | (shared) |
| `BytesSent` | (shared) | (shared) |
| `PacketsInUse` | (shared) | (shared) |

---

## Configuration Comparison

```bash
BattleServer.exe ^
  -name "My Server" ^
  ├─ bsPort 27012 ← Native clients (direct TCP, unencrypted)
  ├─ webSocketPort 9090 ← Web clients (TCP + TLS + WebSocket)
  ├─ sslCert server.crt ← Only for webSocketPort
  ├─ sslKey server.key ← Only for webSocketPort
  ├─ outOfBandPort 27013 ← OOB ping
  └─ relayBroadcastPort 27014 ← Presence broadcast
```

---

## Use Cases

### bsPort (27012)

- **Native games** (C++, C#, etc.)
- **Desktop applications**
- **Low latency connections** (no TLS overhead)
- **The game's official client**

### webSocketPort (9090)

- **Web browsers** (JavaScript WebSocket)
- **HTML5 web applications**
- **Connections that require encryption** (wss://)
- **Environments with firewall restrictions** (standard port)

---

## Summary

| Aspect | bsPort | webSocketPort |
|---------|--------|---------------|
| **Data carried** | The same messages | The same messages |
| **Sessions** | Shared | Shared |
| **Mixed peers** | Yes | Yes |
| **Processing** | The same ProcessPacket | The same ProcessPacket |
| **Replay data** | The same | The same |
| **Encryption** | No | TLS |
| **Framing** | No | RFC 6455 |
| **Overhead** | Minimal | High (TLS + WS) |

**Conclusion**: Both ports are **functionally equivalent** for carrying game data. The main difference is the transport (direct TCP vs TCP+TLS+WebSocket), but the application protocol (RLink messages) is identical.

---

## See Also

- [bsPort.md](bsPort.md) — Full documentation of bsPort
- [webSocketPort.md](webSocketPort.md) — Full documentation of webSocketPort
- [oob.md](oob.md) — Out-of-Band ping service
- [relay.md](relay.md) — Presence broadcast
- [README.md](README.md) — General project description
