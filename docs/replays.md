# Replay System - Extensive Documentation

> **Historical.** This document describes the replay pipeline of the original
> `BattleServer.exe`. **This implementation does not upload replays**: the
> replay-upload flags (`-uploadVia`, `-replayHost`, `-replayPort`, `-login`,
> `-password`, `-replayDBName`, `-odbcDriver`) are still accepted and are ignored,
> and the server never opens a connection to a replay host. The in-process
> `ReplayDataHandler` callback is the remaining way a host receives recorded data.
> What follows is kept because it is the only record of the request builder.

## General Description

BattleServer.exe's replay system captures the network traffic of matches in real time, compresses it, encodes it in Base64 and sends it by HTTP POST to a replay server. **They are not the player's raw inputs**, but **whole GameDataMessages** (serialised game synchronisation messages).

---

## System Architecture

### Main Components

| Component | C++ Class | Ghidra Function | Purpose |
|------------|-----------|----------------|-----------|
| **ReplayUploadManager** | `ReplayUploadManager` | `FUN_14010fa70` | Manages the replay queue, processes and packages data |
| **WorldwideReplayUpload** | `WorldwideReplayUpload` | `FUN_140110f80` | Builds and sends the HTTP POST with the replay data |
| **ReplayUploadInfo** | `WWBS::ReplayUploadInfo` | `FUN_140110860` | Data structure holding one replay chunk |
| **Session** | `RLink::Session` | `FUN_1400eb5f0` | Manages game sessions, holds per-peer queues |
| **Server** | `Server` | `FUN_1400d81e0` | Main loop, invokes the ReplayUploadManager's Process |
| **ConvertBinaryToString** | — | `FUN_14010df00` | Compresses with zlib and encodes in Base64 |
| **Socks::Send** | `Socks` | `FUN_140092750` | Sends data over the TCP socket |

---

## Replay Binary Structure (ReplayUploadInfo)

Size: `0x20018` bytes (131,096 bytes = ~128KB)

```
Offset      Size      Field                       Type
─────────────────────────────────────────────────────────────
+0x00000    1 byte      State flag                 byte
                                        (0 = empty, 1 = in use)
+0x00001    7 bytes     Padding/reserved           —
+0x10000    8 bytes     sessionID                  int64 (initially -1)
+0x10008    4 bytes     dataLength                 uint32 (uncompressed size)
+0x1000C    4 bytes     sequenceID                 uint32 (sequence number)
+0x10010    ~127KB      Base64 data                char[] (null-terminated)
+0x20010    4 bytes     Final Base64 size          uint32
```

### Structure Initialisation (FUN_140110860)

```c
// Reserves 0x20018 bytes
plVar8 = malloc(0x20018);

// Field initialisation
*(byte *)plVar8 = 0;                    // State flag = empty
plVar8[0x2000] = -1;                    // sessionID = -1 (offset +0x10000)
plVar8[0x2001] = 0;                     // dataLength = 0
*(byte *)(plVar8 + 0x2002) = 0;         // padding
*(uint32 *)(plVar8 + 0x4002) = 0;       // sequenceID = 0
```

### ReplayUploadInfo Pool

The ReplayUploadManager keeps a pool (vector) of ReplayUploadInfo structures:

```
ReplayUploadManager
  +0x08  → Pointer to WorldwideReplayUpload
  +0x10  → End of the pool vector
  +0x18  → Timer (last time a connection was attempted)
  +0x20  → Vector of ReplayUploadInfo (pool)
  +0x28  → Vector of ReplayUploadInfo (send queue)
  +0x30  → End of the send queue
```

---

## Complete Data Flow

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│ 1. PLAYER SENDS A COMMAND                                       │
│    PEERMESSAGE (type 0xF)                                       │
│    Subtype 9  = GAMEDATA                                        │
│    Subtype 0x17 = GAMEDATA_CHECKSUM                              │
│    Subtype 0x10 = PROXY                                          │
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 2. SERVER RECEIVES A MESSAGE                                    │
│    Server::ProcessPacket (FUN_1400de050)                        │
│    - Parses the message type from byte[0] of the buffer         │
│    - GAMEDATA (9): deserialises GameDataMessage                 │
│      · Reads 1 byte: messageType (0xFE = replay data)           │
│      · Reads 2 bytes: dataLength (ushort)                       │
│      · Reads N bytes: binary payload                            │
│    - PROXY (0x10): forwards to PackReplayData                   │
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 3. THE SESSION STORES IT IN A PER-PEER QUEUE                    │
│    Session::OnSyncMessageFromPeer (FUN_1400e2e00)               │
│    - Per-peer queue (max 300 packets, max 512KB)                │
│    - Stores the structure {ptr, size, timestamp}                │
│    - Updates the last timestamp (QueryPerformanceCounter ms)     │
│    - Checks: messageType == 0xFE (-2)                           │
│    - Checks: observer mode flag                                 │
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 4. PERIODIC PROCESSING                                          │
│    FUN_1400ea460 (called from Server::Process)                  │
│    - Checks whether the queue has data and is not full          │
│    - Takes a ReplayUploadInfo from the pool                     │
│    - Assigns sessionID and sequenceID                            │
│    - Calls ReplayUploadManager::OnAddReplayData                 │
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 5. DATA TRANSFORMATION                                          │
│    ReplayUploadManager::OnAddReplayData (FUN_14010fa70)         │
│    └→ ConvertBinaryToString (FUN_1401103f0)                     │
│       └→ FUN_14010df00:                                         │
│          Step 1: ZLIB COMPRESSION (level 9, window=15, mem=8)   │
│          Step 2: BASE64 ENCODING (standard A-Za-z0-9+/= alphabet)│
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 6. PACKING                                                      │
│    PackReplayData (FUN_14010f4f0)                               │
│    - Writes sessionID at +0x10000                               │
│    - Writes seq at +0x1000C                                     │
│    - Writes the base64 size at +0x20010                         │
│    - The base64 data is at +0x10010                             │
└────────────────────────┬────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ 7. HTTP SEND                                                    │
│    WorldwideReplayUpload::PushReplayData (FUN_140110f80)        │
│    - Builds the HTTP POST:                                      │
│      POST /game/replay/setReplayData?matchid={id}&sequenceid={s}│
│      Host: {replayHost}                                         │
│      Content-Length: {base64_size}                              │
│      {base64_data}                                              │
│    - Sends header + body over the TCP socket                    │
└─────────────────────────────────────────────────────────────────┘
```

---

## Function Detail

### 1. Server::ProcessPacket (FUN_1400de050)

**Address**: `0x1400de050`
**Source file**: `Server.cpp`
**Called from**: `FUN_1400d8e10` (network processing)

**Purpose**: Parses incoming messages and dispatches by type.

**Messages relevant to the replay**:

| Type ID | Message | Subtype | Action |
|---------|---------|---------|--------|
| `0x09` | `GameDataMessage` | — | Extracts messageType, dataLength, payload. If messageType == 0xFE, sends to Session::OnSyncMessageFromPeer |
| `0x10` | `ProxyMessage` | — | Forwards to PackReplayData with the proxy data |
| `0x17` | `GameDataAndChecksumMessage` | — | Processes the checksum and forwards the sub-packet |

**Parsing logic for GAMEDATA (type 9)**:

```c
// Deserialises GameDataMessage
// Reads from the binary buffer:
byte messageType = ReadByte();      // Must be 0xFE (-2) for replay data
ushort dataLength = ReadUShort();   // Payload length
byte[] payload = ReadBytes(dataLength);  // Raw binary data

// If messageType == 0xFE and there is an active session:
if (messageType == -2 && session != NULL) {
    Session::OnSyncMessageFromPeer(session, fromPeerID, &gameDataMessage);
}
```

**Logic for PROXY (type 0x10)**:

```c
// ProxyMessage contains:
byte sourcePeerID = ReadByte();
vector<byte> destinationPeerIDs = ReadVector();
byte[] data = ReadRemaining();

// Gets Server[0x170] (ReplayUploadManager) and Server[0x2a4] (flag)
// Calls PackReplayData with the proxy data
FUN_14010f4f0(ReplayUploadManager, &session, sourcePeerID, ...);
```

---

### 2. Session::OnSyncMessageFromPeer (FUN_1400e2e00)

**Address**: `0x1400e2e00`
**Called from**: `FUN_1400de050`

**Purpose**: Stores GameDataMessages in the per-peer queue for later processing as a replay.

**Relevant Session offsets**:

| Offset | Field | Type | Description |
|--------|-------|------|-------------|
| `+0x18` | PeerQueues | `array[256]` | Queue of messages per peer ID |
| `+0x830` | titleID | `short` | Game ID |
| `+0x838` | sessionID | `int64` | Unique session ID |
| `+0x848` | ReplayUploadInfo | `ptr` | Pointer to the active replay info |
| `+0x850` | RefCount | `shared_ptr` | Reference counter |
| `+0x858` | SeqCounter | `uint32` | Sequence counter |
| `+0x85d` | ObserverMode | `byte` | Observer mode flag |
| `+0x890` | QueueBegin | `ptr` | Start of the peer queue |
| `+0x898` | QueueEnd | `ptr` | End of the peer queue |

**Queueing logic**:

```c
// Verifies the conditions:
// 1. messageType == 0xFE (simulation data)
// 2. The peer's queue exists (Session + 0x18 + peerID * 8)
// 3. Not in observer mode (or the peer has a special flag)

// Stores in the queue:
// - Maximum 300 packets per peer
// - Maximum 512KB (0x80000) per peer
// - Structure: {data_ptr, size, timestamp}
// - Timestamp: QueryPerformanceCounter in ms (FUN_14010f310)
```

---

### 3. FUN_1400ea460 (Periodic Processing)

**Address**: `0x1400ea460`
**Called from**: `FUN_1400d8e10`

**Purpose**: Checks the peer queues and triggers replay processing.

**Logic**:

```c
// Iterates over all active sessions
// For each session:
//   1. Checks whether the peer queue has data
//   2. Checks whether the queue is not full
//   3. Takes a ReplayUploadInfo from the pool (FUN_140110860)
//   4. Assigns sessionID (from Session + 0x838)
//   5. Assigns sequenceID (increments Session + 0x858)
//   6. Calls ReplayUploadManager::OnAddReplayData
```

---

### 4. ReplayUploadManager::OnAddReplayData (FUN_14010fa70)

**Address**: `0x14010fa70`
**Source file**: `cohoreplaytool/Src/ReplayUploadManager.cpp`
**Called from**: `FUN_1400ea460`

**Purpose**: Main callback that transforms binary data into a sendable replay format.

**Parameters**:
- `param_1`: Pointer to the ReplayUploadManager
- `param_2`: Pointer to the ReplayUploadInfo (from the pool)
- `param_3`: Flag (1 = normal processing)

**Flow**:

```
1. Log: "OnAddReplayData - ConvertBinaryToString"
2. Records the timestamp (FUN_14010f310)
3. Gets the pointer to the ReplayUploadInfo: *param_2
4. Calls ConvertBinaryToString:
   - Input: binary data at offset +0x10010
   - Size: dataLength from offset +0x10008
   - Output: Base64 at offset +0x10010
5. On failure: log "ReplayUploadManager::OnAddReplayData failed ConvertBinaryToString sessionID: seq: dropped"
6. Computes the Base64 size (strlen up to the null)
7. Stores the size at +0x20010
8. Calls WorldwideReplayUpload's vtable+0x30 (PushReplayData)
9. Processes the response: gets the vector of ReplayUploadInfos for sending
10. Adds to the send queue (ReplayUploadManager + 0x28)
```

**Error handling**:

| Error | Message | Action |
|-------|---------|--------|
| Conversion failed | `"ReplayUploadManager::OnAddReplayData failed ConvertBinaryToString sessionID: seq: dropped"` | Log + drop the replay |
| Share count != 1 | `"retireReplayInfo[%d] share count (%u) is not 1"` | C++ exception |

---

### 5. ConvertBinaryToString (FUN_14010df00)

**Address**: `0x14010df00`
**Source file**: `ClientLib/rlink/internal/core/StringHelpers.cpp`
**Called from**: `FUN_1401103f0`

**Purpose**: Compresses binary data with zlib and encodes it in Base64.

**Parameters**:
- `param_1`: Output buffer (Base64)
- `param_2`: Maximum output size
- `param_3`: Intermediate buffer (compression)
- `param_4`: Maximum intermediate size
- `param_5`: Input data (binary)
- `param_6`: Input size

**Step 1: Zlib Compression**

```c
// FUN_140078660 → deflate (zlib 1.2.11)
int result = deflate(
    param_3,        // Compressed buffer
    &param_6,       // Compressed size (output)
    param_5,        // Original data
    param_4,        // Original size
    9               // Compression level (maximum)
);
// Window bits: 15
// Mem level: 8
// Strategy: default (0)
```

**Step 2: Base64 Encoding**

```c
// FUN_1400ecc10
// Alphabet: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
// Appends a null terminator at the end
// Handles padding with '='
```

**Base64 algorithm (FUN_1400ecc10)**:

```c
// Processes 3 input bytes → 4 output bytes
// For each group of 3 bytes:
output[0] = alphabet[input[0] >> 2];
output[1] = alphabet[((input[0] & 3) << 4) | (input[1] >> 4)];
output[2] = alphabet[((input[1] & 0xF) << 2) | (input[2] >> 6)];
output[3] = alphabet[input[2] & 0x3F];

// If 1 or 2 bytes are left: padding with '='
// Null-terminates the result
```

---

### 6. PackReplayData (FUN_14010f4f0)

**Address**: `0x14010f4f0`
**Source file**: `cohoreplaytool/Src/ReplayUploadManager.cpp`
**Called from**: `FUN_1400de050` (PROXY messages)

**Purpose**: Packages proxy/relay data into a ReplayUploadInfo structure.

**Parameters**:
- `param_1`: ReplayUploadManager
- `param_2`: Session*
- `param_3`: fromPeerID
- `param_4`: dataLength
- `param_5`: data buffer
- `param_6`: buffer size
- `param_7`: Server[0x2a4] (activation flag)

**Logic**:

```c
// 1. Checks whether the replay queue is available
// 2. Takes a ReplayUploadInfo from the pool (FUN_140110860)
// 3. Gets sessionID from Session + 0x838
// 4. Gets sequenceID from Session + 0x858 (increments it)
// 5. Writes into the ReplayUploadInfo:
//    +0x10000 = sessionID
//    +0x1000C = sequenceID
//    +0x10008 = dataLength
//    +0x10010 = binary data
// 6. Adds to the send queue
```

---

### 7. WorldwideReplayUpload::PushReplayData (FUN_140110f80)

**Address**: `0x140110f80`
**Source file**: `cohoreplaytool/Src/WorldwideReplayUpload.cpp`
**Called from**: `FUN_14010fa70` (vtable +0x30)

**Purpose**: Builds and sends the HTTP POST with the replay data.

**Parameters**:
- `param_1`: WorldwideReplayUpload
- `param_2`: ReplayUploadInfo*
- `param_3`: Send queue

**Send condition**:

```c
// Only sends if the socket state ∈ [6, 10]
// State stored at: *(short *)(*(longlong *)(param_1 + 0x58) + 0x38)
short state = *(short *)(*(longlong *)(worldwideReplayUpload + 0x58) + 0x38);
if (state >= 6 && state <= 10) {
    // Proceeds with the send
}
```

**Building the HTTP Request**:

```c
// Query string
snprintf(buf, size, "%s%llu%s%u",
    "?matchid=",
    replayInfo->sessionID,    // +0x10000
    "&sequenceid=",
    replayInfo->seq);         // +0x1000C

// HTTP headers
snprintf(buf, size, "%s%s%s%s%s%s%s%zu%s",
    "POST /game/replay/setReplayData",
    query_string,
    " HTTP/1.1\r\n",
    "Host: ",
    replayHost,               // WorldwideReplayUpload + 0x08
    "\r\n",
    "Content-Length: ",
    replayInfo->dataLength,   // +0x20010
    "\r\n\r\n");
```

**Send**:

```c
// Send the headers
FUN_140092750(socket_manager, connection, httpHeader);
// Send the body (base64 data)
FUN_140092750(socket_manager, connection, replayData, dataLength);
```

**Error handling**:

| Error | Message | Action |
|-------|---------|--------|
| Headers fail | `"WorldwideReplayUpload::PushReplayData - SockSend httpHeader has fatal error"` | C++ exception |
| Body fails | `"WorldwideReplayUpload::PushReplayData - SockSend batchedReplayData has fatal error"` | C++ exception |

---

### 8. ReplayUploadManager::Process (FUN_140110380)

**Address**: `0x140110380`
**Called from**: `FUN_1400d81e0` (Server::Process)

**Purpose**: Main loop of the replay system, called on every server tick.

**NOTE (CORRECTED)**: Earlier documentation claimed that `param_3` (Server[0x2a4]) was hardcoded to 0 and blocked the function. **Wrong**: the flag is initialised to **1** (`sub_1400D6790.c:158`). The real cause of the "connect deferred" is that the replay socket **never receives `AnimateAll`** (it only gets a ring-buffer read via vtable+0x28), getting stuck in state 3. See [uploadVia-lobby-connection.md](uploadVia-lobby-connection.md).

**Logic**:

```c
void ReplayUploadManager::Process(longlong this, longlong time, int serverFlag) {
    // serverFlag = Server[0x2a4] = 1 (initialised in the constructor; NOT a bug)
    if (serverFlag != 0 && *(longlong **)(this + 8) != NULL) {
        // This block DOES run
        
        // Every 5000ms: attempts to connect
        if (time - *(longlong *)(this + 0x18) > 5000) {
            *(longlong *)(this + 0x18) = time;
            // vtable +0x10 → Connect
            char result = (**(code **)(**(longlong **)(this + 8) + 0x10))();
            if (result == 0) {
                *(longlong *)(this + 0x18) = 0;
                return;
            }
        }
        
        // Every tick: AnimateAll (process the queue)
        // vtable +0x28 → ProcessQueue
        (**(code **)(**(longlong **)(this + 8) + 0x28))(*(longlong **)(this + 8), time);
    }
}
```

**ReplayUploadManager offsets**:

| Offset | Field | Type | Description |
|--------|-------|------|-------------|
| `+0x08` | WorldwideReplayUpload | `ptr` | Send interface |
| `+0x10` | PoolEnd | `ptr` | End of the ReplayUploadInfo pool |
| `+0x18` | Timer | `int64` | Last connection timestamp |
| `+0x20` | Pool | `vector` | Pool of available structures |
| `+0x28` | SendQueue | `vector` | Send queue |
| `+0x30` | SendQueueEnd | `ptr` | End of the send queue |

---

### 9. WorldwideReplayUpload::Connect (FUN_140110f10)

**Address**: `0x140110f10`
**Called from**: vtable +0x10

**Purpose**: Starts the TCP connection to the replay server.

```c
void Connect(longlong this) {
    char *host = (char *)(this + 0x8);  // replayHost
    short port = *(short *)(this + 0x28); // replayPort
    // Calls Socks::Connect
    FUN_140091050(this + 0x30, this + 0x58, host, port);
}
```

---

### 10. Socks::Send (FUN_140092750)

**Address**: `0x140092750`
**Source file**: `ClientLib/rlink/networking/sock.cpp`

**Purpose**: Sends data over the socket with state verification.

**Logic**:

```c
int Socks::Send(longlong this, longlong socket, char *data, int length) {
    if (data == 0 || length < 1) return 1;
    
    // Verifies the socket state
    // If state > 3 but flag +0x100 == 0: failure
    if (*(int *)(socket + 0x100) == 0 && 
        (*(short *)(socket + 0x38) - 3) > 3) {
        *(int *)(this + 0x10) = -1;
        return 0;
    }
    
    // Buffers the data
    char result = BufferData(socket + 0x40, data, length);
    if (result) {
        // Sends (with SSL if configured)
        FUN_1400968d0(this, socket);
        return 1;
    }
    return 0;
}
```

---

## GameData Message Structure

### GameDataMessage (type 0x09)

```
Field           Size    Type        Description
────────────────────────────────────────────────────────────
messageType     1 byte    byte        0xFE = simulation data
dataLength      2 bytes   ushort      Payload length
payload         N bytes   byte[]      Serialised game state
```

### GameDataAndChecksumMessage (type 0x17)

```
Field           Size    Type        Description
────────────────────────────────────────────────────────────
messageType     1 byte    byte        Identifier
dataLength      2 bytes   ushort      Length
payload         N bytes   byte[]      Data
checksum        8 bytes   uint64      Verification checksum
```

### ProxyMessage (type 0x10)

```
Field               Size    Type        Description
────────────────────────────────────────────────────────────
sourcePeerID        1 byte    byte        Origin peer ID
destinationPeerIDs  variable  vector      Destination peer IDs
data                variable  byte[]      Sub-packet data
```

---

## Data Transformation

### Complete Pipeline

```
Raw Binary Data (GameDataMessages)
        │
        ▼
┌─────────────────────────────────────────────┐
│ ZLIB COMPRESSION                            │
│ · Algorithm: deflate                        │
│ · Level: 9 (maximum)                        │
│ · Window bits: 15                           │
│ · Mem level: 8                              │
│ · Strategy: default (0)                     │
│ · Implementation: zlib 1.2.11               │
│ · Source: functions/inflate_fast.c, etc.    │
└─────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────┐
│ BASE64 ENCODING                             │
│ · Alphabet: A-Za-z0-9+/=                    │
│ · RFC: 4648                                 │
│ · Padding: '='                              │
│ · Null-terminated                           │
│ · Source: functions/FUN_1400ecc10.c         │
└─────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────┐
│ HTTP PACKING                                │
│ · Method: POST                              │
│ · Path: /game/replay/setReplayData          │
│ · Query: ?matchid={id}&sequenceid={seq}     │
│ · Headers: Host, Content-Length             │
│ · Body: base64 data                         │
│ · No encryption (plain HTTP)                │
└─────────────────────────────────────────────┘
```

### No Encryption

The replay data is sent in the clear over HTTP. Only the client's WebSocket connections use TLS (wss://). The replay system has no additional encryption layer.

---

## Socket Connection State

### State Machine

The replay upload socket (inside WorldWideReplayUpload + 0x30) has a state machine stored at `*(short *)(socket + 0x38)`:

```
State  Meaning                  Does it send?
─────────────────────────────────────────────────
0       Disconnected             No — tries to reconnect
1       Resolving DNS            No
2       Listening (server)        No
3       CONNECTING               No — WSAEWOULDBLOCK
4       Needs DNS resolve        No
5       Reserved                 No
6       CONNECTED (ready)        **Yes**
7       Ready to send            **Yes**
8       TLS shutdown             **Yes**
9       Extended state           **Yes**
10      Extended state           **Yes**
```

### Conditions for Sending

```c
// In PushReplayData:
if (state >= 6 && state <= 10) {
    // Sends the HTTP POST
}

// In Socks::Send:
if (state > 3 && flag_0x100 == 0) {
    // Fails: the handshake is not complete
    return 0;
}
```

---

## ReplayUploadManager Configuration

### Command Line Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `-uploadVia` | enum | — | Must be `lobby` for HTTP upload |
| `-replayHost` | string | — | Host of the replay HTTP server |
| `-replayPort` | uint | — | Port of the replay HTTP server |
| `-maxPagesToStorePerSession` | uint | — | Max pages per session |
| `-pageSize` | uint | `65536` | Page size (64KB) |

### Initialisation in Server (FUN_1400d6790)

```c
// Server constructor (FUN_1400d6790)
// When uploadVia == 2 (lobby):

// Allocates 0x70 bytes for WorldwideReplayUpload
worldwideReplayUpload = malloc(0x70);

// Constructor with parameters:
FUN_1400d3950(
    worldwideReplayUpload,
    replayHost,      // from -replayHost
    replayPort,      // from -replayPort
    0,               // login (NULL for lobby)
    0,               // password (NULL for lobby)
    0                // replayDBName (NULL for lobby)
);

// Stores at Server[0x170]
*(longlong *)(server + 0x170) = worldwideReplayUpload;

// CORRECTED (see uploadVia-lobby-connection.md): the flag is NOT hardcoded to 0
*(uint32 *)(server + 0x2a4) = 1;  // Initialised to 1 at sub_1400D6790.c:158
```

### Relevant Server Offsets

| Offset | Field | Type | Description |
|--------|-------|------|-------------|
| `+0x168` | LobbySocksClient | `ptr` | Heartbeat client |
| `+0x170` | ReplayUploadManager | `ptr` | Replay manager |
| `+0x180` | LobbySocksClient2 | `ptr` | Secondary lobby client |
| `+0x188` | WebSocksClient | `ptr` | WebSocket client |
| `+0x2a4` | ReplayFlag | `int` | Activation flag (initialised to 1; the real snag is the missing AnimateAll — see [uploadVia-lobby-connection.md](uploadVia-lobby-connection.md)) |

---

## ReplayUploadInfo Pool

### Pool Management

The ReplayUploadManager keeps a pool of ReplayUploadInfo structures to avoid frequent allocations:

```c
// Pool structure (vector of pairs)
// Each element: {ReplayUploadInfo*, RefCount*}

// Getting from the pool (FUN_140110860):
// 1. If the pool is not empty: takes the last element
// 2. If the pool is empty: allocates a new one (malloc 0x20018)
// 3. Initialises the fields (sessionID = -1, etc.)
// 4. Adds to the pool

// Returning to the pool:
// 1. Resets the state flag to 0
// 2. Adds at the end of the pool
```

### Reference Counter

```c
// Each ReplayUploadInfo has an associated RefCount
// Structure: {vftable, ref_count, ref_count2, ptr_info}

// Increment:
LOCK();
*(int *)(ref_count + 8) = *(int *)(ref_count + 8) + 1;
UNLOCK();

// Decrement with auto-free:
LOCK();
*(int *)(ref_count + 8) = *(int *)(ref_count + 8) - 1;
UNLOCK();
if (ref_count == 1) {
    // Calls the destructor
}
```

---

## Send Queue

### Structure

```c
// ReplayUploadManager + 0x28: send queue (vector)
// Each element: {ReplayUploadInfo*, RefCount*}

// Adding to the queue:
if (queue_end == queue_capacity) {
    // Resizes the vector
    FUN_140110430(manager + 0x28, queue_end, &replay_info);
} else {
    // Adds at the end
    *queue_end = replay_info;
    queue_end++;
}
```

### Send Process

```c
// In PushReplayData, after sending successfully:
// 1. Adds the ReplayUploadInfo to the send queue
// 2. The next Process() cycle will process the queue
// 3. Once sent, returns the ReplayUploadInfo to the pool
```

---

## Error Handling

### Known Errors

| Error | Source | Cause | Action |
|-------|--------|-------|--------|
| `"ReplayUploadManager::OnAddReplayData failed ConvertBinaryToString sessionID: seq: dropped"` | FUN_14010fa70:103 | Compression/Base64 failure | Log + drop the replay |
| `"WorldwideReplayUpload::PushReplayData - SockSend httpHeader has fatal error"` | FUN_140110f80:43 | Failure sending the HTTP headers | C++ exception |
| `"WorldwideReplayUpload::PushReplayData - SockSend batchedReplayData has fatal error"` | FUN_140110f80:51 | Failure sending the HTTP body | C++ exception |
| `"retireReplayInfo[%d] share count (%u) is not 1"` | FUN_14010fa70:295 | Invalid reference counter | C++ exception |
| `"Disconnected From Replay Server, Reconnecting..."` | FUN_140110b90 | Connection lost | Automatic reconnection |
| `"connect exceeded timeout"` | — | Connection timeout (10s) | Retry in 5000ms |

### Behaviour on Errors

- **Original behavior: no retry**: If the original HTTP send fails, the replay is
  lost. The Go reimplementation used to retry once after a connection or send
  failure; that uploader has since been removed, so this server does not upload at
  all.
- **C++ exception**: `_CxxThrowException` propagates the error
- **The server keeps working**: only that specific upload fails
- **Automatic reconnection**: If the TCP connection is lost, it tries to reconnect every 5000ms

---

## Integration with Server::Process

### Main Loop (FUN_1400d81e0)

```c
void Server::Process(longlong this, longlong time) {
    // Processes the lobby heartbeat
    FUN_1400e0de0(time, Server[0x180], Server+0x190, Server[0x140]);
    
    // Processes WebSocket
    FUN_1400e0de0(time, Server[0x188], Server[0x198], Server[0x140]);
    
    // Processes the replay upload (see the note: flag 0x2a4 = 1; the real snag is the missing AnimateAll)
    FUN_140110380(Server[0x170], time, Server[0x2a4]);
    //                                    ^^^^^^^^^^^^
    //                                    Initialised to 1 (not a bug)
}
```

### Call Chain

```
Server::Process (FUN_1400d81e0)
  └→ ReplayUploadManager::Process (FUN_140110380)
       ├→ [Every 5000ms] vtable+0x10 → Connect (FUN_140110f10)
       │    └→ Socks::Connect (FUN_140091050)
       └→ [Every tick] vtable+0x28 → ProcessQueue
            └→ PushReplayData (FUN_140110f80)
                 ├→ SockSend(httpHeader)
                 └→ SockSend(replayData)
```

---

## Replay Content Summary

### What is Captured?

| Data | Included | Format |
|------|----------|---------|
| Whole GameDataMessages | Yes | Serialised binary |
| Timestamps | Yes | QueryPerformanceCounter (ms) |
| sessionID | Yes | int64 |
| titleID | Yes | int16 |
| sequenceID | Yes | uint32 |
| fromPeerID | Yes | byte |
| packetNumber | Yes | uint32 |
| ackBits/flagBits | Yes | uint32 |
| Checksums | Yes | uint64 (GameDataAndChecksum) |
| The player's direct inputs | **No** | Indirectly via GameDataMessages |

### What is NOT Captured?

- The player's raw inputs (keyboard/mouse)
- Full world state (only changes/deltas)
- Audio/video data
- Chat or text messages
- Authentication information

---

## Cross References

### Identified Source Names

The names below are original/upstream source names. They are not all local
paths; the local evidence is the address-based decompilation under `src/`.

| Component | Original Source File |
|------------|------------------------|
| Server | `Server.cpp` |
| ReplayUploadManager | `cohoreplaytool/Src/ReplayUploadManager.cpp` |
| WorldwideReplayUpload | `cohoreplaytool/Src/WorldwideReplayUpload.cpp` |
| SessionManager | `SessionManager.cpp` |
| SessionMessage | `SessionMessage.cpp` |
| StringHelpers | `ClientLib/rlink/internal/core/StringHelpers.cpp` |
| Sock | `ClientLib/rlink/networking/sock.cpp` |
| RLink networking | `ClientLib/rlink/networking/` |

### Build System

- **CI/CD**: Azure DevOps (`D:\a\1\s\`)
- **Compiler**: MSVC (Visual Studio 2019 Release)
- **zlib**: v1.2.11 (bundled)
- **Partial source code**: `cohoreplaytool/`, `ClientLib/rlink/`

---

## See Also

- [uploadVia-lobby.md](uploadVia-lobby.md) — General documentation of the HTTP method
- [uploadVia-lobby-connection.md](uploadVia-lobby-connection.md) — Diagnosis of the connection bug
- [uploadVia-lobby-state.md](uploadVia-lobby-state.md) — Connection states
- [LOBBY-PROTOCOL.md](LOBBY-PROTOCOL.md) — Heartbeat system (independent)
- [README.md](README.md) — General project description
