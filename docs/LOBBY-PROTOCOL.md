# Lobby Protocol (Heartbeat and Registration)

## General Description

The BattleServer establishes a **heartbeat connection** with the lobby server in order to:
- Register the server as available
- Keep the connection alive with periodic heartbeats
- Manage the creation of game sessions
- Process relay messages between peers

### IMPORTANT: this system is INDEPENDENT of Replay Upload

The heartbeat/lobby connection (`lobbyHost:lobbyPort`) **has no relation whatsoever** to the
replay upload system (`-uploadVia`). They are two completely separate subsystems:

- **Lobby Heartbeat** → server registration, session management, relay between peers
- **Replay Upload** → replay upload (HTTP POST / MySQL / ODBC) in the original; **not
  implemented in this server**, where the flags are accepted and ignored and no connection is
  opened

**There is no dependency**: the upload works without the lobby, and the lobby works without the upload. The term "lobby" in `uploadVia=lobby` is only a historical name for the worldwide HTTP method.

See the details in [uploadVia-lobby.md](uploadVia-lobby.md)

---

## Lobby Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `-lobbyHost` | string | `"localhost"` | **Heartbeat/lobby server host** |
| `-lobbyPort` | uint | `8443` (0x20FB) | **Heartbeat/lobby server port** |
| `-clusterName` | string | — | Cluster identifier for the lobby |
| `-heartbeatSecurityKey` | string | — | Authentication key for the heartbeat |
| `-publicIP` | string | — | Public IP advertised to clients |
| `-publicPort` | uint | — | Public port advertised to clients |
| `-region` | string | `"global"` | Server region |

---

## Connection Establishment

### Parameters on the Server

Stored in `FUN_1400d6790.c`:
- `server+0x210` → `lobbyHost` (string)
- `server+0x230` → `lobbyPort` (short)

### Connection Type

```c
socket(AF_INET, SOCK_STREAM, IPPROTO_TCP)  // socket(2, 1, 6)
connect()  // to lobbyHost:lobbyPort
```

It is a standard TCP connection. There is no TLS encryption on this connection (unlike WebSocket with `wss://`).

---

## Lobby Protocol Messages

### Message Types (from RLink::SessionMessage)

| Type ID | Message | Direction | Description |
|---------|---------|-----------|-------------|
| `0x13` | `LobbyRelayMessage` | Bidirectional | Data relay between peers via the lobby |
| `0x14` | `LobbyPingMessage` | Bidirectional | Lobby ping/keepalive |
| `0x15` | `LobbyConnectMessage` | Server → Lobby | Initial connection and registration |

### LobbyConnectMessage (0x15)

Sent when establishing the connection with the lobby:

| Field | Offset | Type | Default | Description |
|-------|--------|------|---------|-------------|
| `titleID` | +0x30 | short | 0xffff | Game ID |
| `sessionID` | +0x38 | longlong | -1 | Session ID |
| `secret` | +0x48 | ulonglong | -1 | Session secret |
| `dataReceivedCount` | +0x54 | int | -1 | Received data counter |
| `protocolVersion` | +0x58 | short | 5 | Protocol version |
| `flags` | +0x5c | int | 0 | Connection flags |

**Log**: `"Server::ProcessPacket: fromPeerID=%d LOBBYCONNECT titleID=%d sessionID=%lld secret=%llu dataReceivedCount=%d"`

### LobbyRelayMessage (0x13)

Data relay between peers through the lobby:

| Field | Type | Description |
|-------|------|-------------|
| `fromPeerID` | byte | Origin peer ID |
| `toPeerID` | byte | Destination peer ID |
| `data` | bytes | Sub-packet data |

**Log**: `"Server::ProcessPacket: fromPeerID=%d LOBBYRELAY fromPeerID=%d toPeerID=%d"`

### LobbyPingMessage (0x14)

Simple keepalive:

**Log**: `"Server::ProcessPacket: fromPeerID=%d LOBBYPING"`

---

## Other Lobby-Related Messages

### ProxyMessage (0x10)

Peer proxy message:

| Field | Type | Description |
|-------|------|-------------|
| `sourcePeerID` | byte | Source peer ID |
| `destinationPeerIDs` | array | Destination peer IDs |
| `data` | bytes | Message data |

**Log**: `"Server::ProcessPacket: fromPeerID=%d PROXY sourcePeerID=%d destinationPeerIDs=[%s]"`

---

## Authentication with the Lobby

### Process

1. **TCP connection** to `lobbyHost:lobbyPort`
2. **Sending the LobbyConnectMessage** with:
   - `heartbeatSecurityKey` as the shared key
   - `sessionID` and `secret` for validation
   - `protocolVersion = 5`
3. **The lobby's response** accepts or rejects

### Error Messages

| Error | Context |
|-------|----------|
| `"ProcessPacket - LOBBYRELAY srcPeer id shouldn't be invalid."` | Relay validation |
| `"Attempt to reconnect to a non-existent sessionID"` | Invalid reconnection |
| `"SessionManager failed to process peer on connect"` | Connection error |

---

## Message Processing (FUN_1400de050.c)

The server processes incoming lobby messages in `Server::ProcessPacket`:

```
Server::ProcessPacket
  ├→ LOBBYCONNECT (0x15)  → Session registration/creation
  ├→ LOBBYRELAY (0x13)    → Forwarding data to peers
  ├→ LOBBYPING (0x14)     → Keepalive response
  ├→ PROXY (0x10)         → Proxy between peers
  └→ GameData (0x09)      → Game data
```

---

## Structure Offsets

In `FUN_1400d6790.c`, `param_3` points at the parameters structure:

| Offset | Field | Use |
|--------|-------|-----|
| `+0x68` | `lobbyHost` (string) | **Heartbeat only** |
| `+0x78` | `lobbyPort` (short) | **Heartbeat only** |
| `+0xD0` | `clusterName` (string) | Heartbeat |
| `+0xF8` | `publicIP` (string) | Registration |
| `+0x118` | `publicPort` (short) | Registration |

---

## Code References

| Function | Source File | Purpose |
|---------|----------------|-----------|
| `FUN_1400d6790` | `src/sub_1400D6790_1400d6790.cpp` | Lobby parameter setup |
| `FUN_1400de050` | `src/sub_1400DE050_1400de050.cpp` | ProcessPacket — lobby messages |
| `FUN_1400eb5f0` | `src/sub_1400EB5F0_1400eb5f0.cpp` | Session management |
| `FUN_1400ed400` | `src/sub_1400ED400_1400ed400.cpp` | Message factory |
| `FUN_1400e61b0` | `src/sub_1400E61B0_1400e61b0.cpp` | LobbyPing processing |

---

## See Also

- [uploadVia-lobby.md](uploadVia-lobby.md) — Replay upload over HTTP POST (independent system)
- [replays.md](replays.md) — Complete replay system
