# uploadVia lobby - Replay Upload over HTTP POST

> **Historical.** This document describes the `-uploadVia=lobby` method of the
> original `BattleServer.exe`. **This implementation does not upload replays**: the
> flags are accepted and ignored, and no connection is opened to a replay host. The
> HTTP contract below is kept as the record of the original, and the
> `BattleServer.exe` launch examples are the original's, not this server's.

## Description

The `-uploadVia lobby` method sends the replays of the matches by **HTTP POST** to the server configured at `-replayHost:replayPort`.

> **The name is misleading**: the term "lobby" here is a historical convention for the "worldwide" HTTP method. **It has nothing to do** with the heartbeat/lobby connection (`lobbyHost:lobbyPort`). See [LOBBY-PROTOCOL.md](LOBBY-PROTOCOL.md) for the lobby system.

---

## Parameters

| Parameter | Requirement | Description |
|-----------|-----------|-------------|
| `-replayHost` | **Required** | Host of the replay HTTP server |
| `-replayPort` | **Required** | Port of the replay HTTP server |

**Does NOT need**: `-login`, `-password`, `-replayDBName`, `-odbcDriver`, `-lobbyHost`, `-lobbyPort`

---

## HTTP Protocol

### Endpoint

```
POST /game/replay/setReplayData?matchid=<sessionID>&sequenceid=<seq> HTTP/1.1\r\n
Host: <replayHost>\r\n
Content-Length: <length>\r\n
\r\n
<replay data>
```

### Headers Sent

| Header | Value | Required |
|--------|-------|-------------|
| `Host` | Value of `-replayHost` | Yes |
| `Content-Length` | Replay size in bytes | Yes |

**NOT sent**: `Content-Type`, `Authorization`, `User-Agent`, `Cookie`

### HTTP vs HTTPS

**Plain HTTP (without TLS)**. There is no encryption in the replay upload. Only the client's WebSocket connections use `wss://`.

---

## Replay Data Structure

| Offset | Field | Type | Description |
|--------|-------|------|-------------|
| `+0x10000` | `sessionID` | `longlong` | Unique session/match ID |
| `+0x1000c` | `seq` | `uint` | Sequence number of the chunk |
| `+0x10010` | `data` | `byte[]` | Binary replay data |
| `+0x20010` | `dataLength` | `uint` | Length of the data |

---

## Upload Flow

```
1. The Game Session generates replay data
       │
2. ReplayUploadManager::OnAddReplayData
       └→ ConvertBinaryToString()
       └→ Queues for sending
       │
3. WorldwideReplayUpload::PushReplayData
       └→ Builds the query: ?matchid=<id>&sequenceid=<seq>
       └→ Builds the HTTP headers
       └→ SockSend(httpHeader)
       └→ SockSend(replayData)
       │
4. retireReplayInfo() — frees the resource
```

### Upload Timing

- **During the session**, not at the end
- For each generated replay chunk
- Limited concurrency (configurable maximum at the same time)

### Error Handling

- **Original behavior: no retry** — if the original upload fails, the replay is
  lost. This implementation does not upload at all; the uploader that used to retry
  once has been removed.
- A C++ exception is thrown (`_CxxThrowException`)
- The server keeps working (only that upload fails)

---

## Building the HTTP Request (FUN_140110f80.c)

```c
// Query string
snprintf(buf, size, "%s%llu%s%u",
    "?matchid=",
    replayInfo->sessionID,    // +0x10000
    "&sequenceid=",
    replayInfo->seq);         // +0x1000c

// HTTP headers
snprintf(buf, size, "%s%s%s%s%s%s%s%zu%s",
    "POST /game/replay/setReplayData",
    query_string,
    " HTTP/1.1\r\n",
    "Host: ",
    replayHost,               // from -replayHost
    "\r\n",
    "Content-Length: ",
    replayInfo->dataLength,   // +0x20010
    "\r\n\r\n");
```

### Send

```c
FUN_140092750(socket_manager, connection, httpHeader);   // headers
FUN_140092750(socket_manager, connection, replayData);   // body
```

---

## Configuration

### Minimum

```bash
BattleServer.exe \
  -uploadVia lobby \
  -replayHost replay.mycompany.com \
  -replayPort 80 \
  -bsPort 27012
```

### Recommended

```bash
BattleServer.exe \
  -uploadVia lobby \
  -replayHost replay.mycompany.com \
  -replayPort 80 \
  -name "My Server" \
  -bsPort 27012 \
  -webSocketPort 9090 \
  -sslCert server.crt \
  -sslKey server.key \
  -region "eu-west" \
  -observerMax 256
```

---

## Known Errors

| Message | File | Cause |
|---------|---------|-------|
| `"WorldwideReplayUpload::PushReplayData - SockSend httpHeader has fatal error"` | FUN_140110f80.c:43 | Failure sending the headers |
| `"WorldwideReplayUpload::PushReplayData - SockSend batchedReplayData has fatal error"` | FUN_140110f80.c:51 | Failure sending the body |
| `"ReplayUploadManager::OnAddReplayData failed ConvertBinaryToString sessionID: seq: dropped"` | FUN_14010fa70.c:103 | Conversion failed |
| `"retireReplayInfo[%d] share count (%u) is not 1"` | FUN_14010fa70.c:295 | Invalid shared resource |

---

## Code References

| Function | Source | Purpose |
|---------|--------|-----------|
| `FUN_1400d6790` | Server.cpp | Building the WorldwideReplayUpload |
| `FUN_14010fa70` | ReplayUploadManager.cpp | OnAddReplayData callback |
| `FUN_140110f80` | WorldwideReplayUpload | Builds and sends the HTTP POST |
| `FUN_140092750` | sock.cpp (Socks::Send) | Socket send |
| `FUN_14010f4f0` | ReplayUploadManager | PackReplayData |

---

## See Also

- [LOBBY-PROTOCOL.md](LOBBY-PROTOCOL.md) — Heartbeat/lobby connection (independent system)
