# uploadVia lobby - Connection State and Upload Requirements

> **Historical.** This document describes the original `BattleServer.exe`'s replay
> connection state machine. **This implementation does not upload replays**: the
> flags are accepted and ignored, and no connection is opened to a replay host.

## The Problem: A Connection Open With No Data

If you observe that the server opens a TCP connection to `replayHost:replayPort` but sends no data, this is **expected behaviour**. The connection is opened eagerly (at start-up) but data only flows when certain conditions are met.

> **IMPORTANT — Root cause of the "connect deferred" (CORRECTED):** ~~The flag at `Server + 0x2a4` is hardcoded to `0` (FUN_1400d6790.c:86), which completely blocks `ReplayUploadManager::Process`.~~ **Wrong**: the flag is initialised to **1** (`sub_1400D6790.c:158`). The real cause is that the replay socket **never receives `AnimateAll`** (vtable+0x28 is a ring-buffer read, not AnimateAll), getting stuck in state 3 and never advancing to [6,10]. See [uploadVia-lobby-connection.md](uploadVia-lobby-connection.md) for the full details.

---

## Connection State Machine

The replay upload connection has a state machine stored at `*(short *)(state_object + 0x38)` where `state_object = *(longlong *)(worldwideReplayUpload + 0x58)`.

### Known States

| State | Meaning | Does it send? |
|--------|-------------|---------------|
| **0** | Disconnected | No — tries to reconnect |
| **1-3** | Connecting/initialising | No |
| **4-5** | Connected, not ready | No — SockSend fails if flag 0x100 == 0 |
| **6-10** | **Ready for upload** | **Yes** |

### Key Code (FUN_140110f80.c:25)

```c
// PushReplayData ONLY sends if state ∈ [6, 10]
if ((ushort)(*(short *)(*(longlong *)(param_1 + 0x58) + 0x38) - 6U) < 5) {
    // ... builds the HTTP POST and sends it ...
}
// If state < 6, returns immediately without doing anything
```

---

## Why No Data Is Sent

### Reason 1: No replay data has been generated

**The most common reason.** Replays are only generated when there are **active game sessions** with peers connected. If there are no matches in progress, there is no data to send.

```
Active Game Session → peers connected → replay data generated → queued → sent
```

With no active sessions, the replay queue is empty and `PushReplayData` has nothing to send.

### Reason 2: The state machine does not advance to [6, 10]

The state advances via the vtable method `0x10` (Process/Update), called every **5000ms** from `ReplayUploadManager::Process`:

```c
// FUN_140110380.c:14-23
if (5000 < (ulonglong)(param_2 - *(longlong *)(param_1 + 0x18))) {
    *(longlong *)(param_1 + 0x18) = param_2;
    cVar1 = (**(code **)(**(longlong **)(param_1 + 8) + 0x10))();  // vtable 0x10
    if (cVar1 == '\0') {
        *(undefined8 *)(param_1 + 0x18) = 0;
        return;  // <-- EARLY RETURN if not ready
    }
}
```

If the vtable method `0x10` returns 0, the timer is reset and no send is attempted.

### Reason 3: The flag at offset 0x100 is not set

At `FUN_140092750.c:25`:
```c
if ((*(int *)(param_2 + 0x100) == 0) && (3 < (ushort)(*(short *)(param_2 + 0x38) - 3U))) {
    // State > 3 BUT flag 0x100 == 0 → the send FAILS
    *(undefined4 *)(param_1 + 0x10) = 0xffffffff;
    return 0;
}
```

If the state is > 3 (connected) but the flag at `+0x100` is 0, the send fails. This flag appears to indicate "handshake complete".

### Reason 4: Incomplete SSL/TLS handshake

If SSL is configured, the handshake must complete before the state advances. Evidence in `FUN_1400968d0.c`:
- `"Socks::socksWrite: SSL waiting for read operation"` (line 507)
- `"Socks::socksWrite: SSL waiting for write operation"` (line 615)

---

## Complete Lifecycle

```
1. The server starts
   └→ WorldwideReplayUpload constructed (FUN_1400d6790.c:328-378)
   └→ TCP connection opened to replayHost:replayPort
   └→ Initial state = 0 or 1

2. Every 5000ms: ReplayUploadManager::Process (FUN_140110380.c)
   └→ Calls vtable 0x10 (Process/Update) → advances the state
   └→ If it returns 0: resets the timer, retries in 5000ms
   └→ If it returns 1: proceeds to send

3. Every tick: vtable 0x28 (ProcessQueue)
   └→ If state ∈ [6, 10]: processes the send queue
   └→ If there is data in the queue: sends the HTTP POST
   └→ If state < 6: does nothing

4. When there is a game session:
   └→ OnAddReplayData (FUN_14010fa70.c) queues the data
   └→ Converts binary to string
   └→ Attempts an immediate send via vtable 0x30
```

---

## How to Verify That It Works

### 1. Generate game traffic

You need:
- At least one client connected over WebSocket
- An active game session
- Peers sending game data

Without this, there is no replay data to upload.

### 2. Check the server logs

Look for these messages:
- `"Disconnected From Replay Server, Reconnecting......."` → state 0, reconnecting
- `"WorldwideReplayUpload::PushReplayData - SockSend httpHeader has fatal error"` → the send failed
- `"ReplayUploadManager::OnAddReplayData failed ConvertBinaryToString"` → invalid data

### 3. Monitor the connection

- The TCP connection must be ESTABLISHED
- If you see constant reconnections, the replay server is not answering
- If the connection is stable but has no data, there are probably no active sessions

---

## Troubleshooting

| Symptom | Probable Cause | Solution |
|---------|---------------|----------|
| Connection open, no data | No active sessions | Connect clients, start a match |
| The connection opens and closes | replayHost unreachable | Check the firewall, the port, the host |
| "Disconnected... Reconnecting" | The server closed the connection | Check that replayHost accepts and holds the connection |
| "SockSend fatal error" | The state never reached 6 | Check SSL, the handshake, the generated data |
| No replay logs | No data generated | Start a game session with peers |

---

## Minimum Working Configuration

```bash
BattleServer.exe \
  -uploadVia lobby \
  -replayHost your-replay-server.com \
  -replayPort 80 \
  -bsPort 27012 \
  -webSocketPort 9090 \
  -name "Test Server"
```

**You also need:**
- A client connecting over WebSocket to `wss://<server>:9090`
- To start a match with at least 2 peers
- The match to generate game data (replay data)

Without these requirements, the TCP connection will exist but there will be no data to send.

---

## Code References

| Function | Source | Purpose |
|---------|--------|-----------|
| `FUN_140110f80` | `src/WWBS__WorldwideReplayUpload__vmethod_30_140110f80.cpp` | PushReplayData — sends the HTTP POST |
| `FUN_140110380` | address-based decompilation in `src/` | Process — main loop |
| `FUN_140110b90` | address-based decompilation in `src/` | Reconnect callback |
| `FUN_140092750` | address-based decompilation in `src/` | SockSend — send with state verification |
| `FUN_1400968d0` | address-based decompilation in `src/` | socksWrite — write with SSL |
| `FUN_14010fa70` | `src/sub_14010FA70_14010fa70.cpp` | OnAddReplayData — builds the queue |

---

## See Also

- [uploadVia-lobby.md](uploadVia-lobby.md) — General documentation of the method
