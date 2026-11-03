# uploadVia lobby - Why the Connection Stays "Deferred"

> **Historical.** This document diagnoses the original `BattleServer.exe`, which
> opened the replay socket and never completed the connection nor sent a request.
> **This implementation does not upload replays** and does not open that socket at
> all; the flags are accepted and ignored.

## The Problem

You see these logs:
```
Socks::socksConnect: 0/484; hostName=127.0.0.1 ip4Address=7f000001 port=8443
Socks::socksConnect: 0/484 connect deferred (in progress)
```
And then **nothing**. The connection never completes.

---

## NOTE ON EARLIER VERSIONS OF THIS DOCUMENTATION

Earlier documentation claimed that the `Server[0x2a4]` flag was hardcoded to 0 and was the cause of the problem. **That was wrong.** The new decompilation confirms that:

1. `Server[0x2a4]` **is already initialised to 1** in the constructor (`sub_1400D6790.c:158`)
2. The previously documented vtable offsets were wrong
3. The real root cause is different (see below)

---

## Root cause: `AnimateAll` is NEVER called for the replay socket

### The real problem

Unlike the lobby and web SocketsClients (which are processed by `sub_1400e0de0` -> `SockAnimateAll`), the replay socket **never receives an `AnimateAll` call**.

`AnimateAll` (`sub_1400929A0`) is the **ONLY** function that can:
- Run `select()` on the socket to detect when it is writable
- Transition the socket from state 3 ("deferred") to state 6 (connected)
- Call `sub_1400983B0`, which confirms the connection

Without `AnimateAll`, the socket stays **stuck in state 3 forever**.

---

## Complete Flow Analysis

### 1. Flag `Server[0x2a4]` — Already 1

```c
// sub_1400D6790.c:158 (Server::Server constructor)
*(_DWORD *)(a1 + 672) = -1;     // offset 0x2a0
*(_BYTE *)(a1 + 676) = 1;      // offset 0x2a4 ← ALREADY 1
*(_DWORD *)(a1 + 680) = 256;    // offset 0x2a8
```

**Conclusion:** the flag is NOT blocked at 0. `ReplayUploadManager::Process` **does run**.

### 2. WorldwideReplayUpload's vtable — the correct offsets

| Offset | Function | Purpose |
|--------|---------|-----------|
| `+0x00` | `sub_1400dcd90` | Destructor/Delete |
| `+0x08` | `sub_140110b60` | QueryInterface/AddRef |
| `+0x10` | `sub_140110b90` | **Reconnect** (NOT Connect) |
| `+0x18` | `sub_140110f10` | **Connect** (Socks::Connect) |
| `+0x20` | `sub_140110f40` | Disconnect/Close |
| `+0x28` | `sub_140111190` | **Ring buffer read** (NOT AnimateAll) |
| `+0x30` | `sub_140110f80` | PushReplayData |
| `+0x38` | `sub_1401111d0` | ProcessQueue |

**Critical finding:** the offset `+0x28` is `sub_140111190` (ring buffer read), **NOT** `AnimateAll` (`sub_1400929a0`).

### 3. ReplayUploadManager::Process — the real flow

```c
// sub_140110380.c (ReplayUploadManager::Process)
void __fastcall sub_140110380(__int64 a1, __int64 a2, int a3)
{
    if ( a3 != 0 ) {                    // a3 = Server[0x2a4] = 1 ✓
        __int64 v5 = *(__int64 *)(a1 + 8);  // v5 = WorldwideReplayUpload
        if ( v5 != 0 ) {
            // a2 = current time (v3 = time in seconds)
            // *(a1 + 24) = timestamp of the last Process call
            
            if ( (a2 - *(longlong *)(a1 + 24)) <= 0x1388  // <= 5000ms?
                || (*(__int64 *)(a1 + 24) = a2,             // Updates the timer
                    (*(byte (__fastcall **)(__int64))(*(__int64 *)v5 + 0x10))(v5) != 0) )
            {
                // vtable+0x10 → sub_140110b90 (Reconnect)
                // Only calls Connect if state == 0, otherwise returns 1
                
                // THIS BLOCK IS CALLED EVERY FRAME:
                (*(void (__fastcall **)(__int64, __int64))(*(__int64 *)v5 + 0x28))(v5, a2);
                // vtable+0x28 → sub_140111190 ← THIS IS A RING BUFFER READ, NOT AnimateAll!
            }
            else {
                *(longlong *)(a1 + 24) = 0;  // Reset the timer
            }
        }
    }
}
```

### 4. AnimateAll — where it is really called

| Manager | Called from | Does it use AnimateAll? |
|---------|---------------|------------------|
| Lobby SocketsClient | `sub_1400e0de0` | Yes — via vtable+0xA8/0xB0 |
| Web SocketsClient | `sub_1400e0de0` | Yes — via vtable+0xA8/0xB0 |
| ReplayUploadManager | `sub_140110380` | **NO** — it only calls the ring buffer read |

`AnimateAll` (`sub_1400929A0`) is only called from:
- `RLink::RawSocketManager__vmethod_A8`
- `RLink::RawSocketManager__vmethod_B0`
- `RLink::WebSocketManager__vmethod_A8`
- `RLink::WebSocketManager__vmethod_B0`

**Never from `WorldwideReplayUpload` nor `ReplayUploadManager`.**

---

## Failure Sequence

```
1. The server starts
   └─ Server[0x2a4] = 1 ✓ (flag correct)

2. Server::Process (sub_1400d81e0:179)
   └─ sub_140110380(ReplayUploadManager, time, 1)  ← flag=1, enters

3. First call (timer = 0, difference >> 5000ms)
   └─ Updates timer = a2
   └─ Calls vtable+0x10 → sub_140110b90 (Reconnect)
       └─ Checks the socket state == 0 ✓
       └─ Calls vtable+0x18 → sub_140110f10 (Connect)
           └─ Socks::Connect → socksConnect
           └─ connect() returns WSAEWOULDBLOCK
           └─ State changes to 3 ("deferred")
           └─ Returns 1 (success)
   └─ Since vmethod_10 returned != 0, enters the if
   └─ Calls vtable+0x28 → sub_140111190 (Ring Buffer Read)
       └─ It does NOTHING to the socket!
       └─ It only reads data from the replay ring buffer

4. Following calls (difference < 5000ms)
   └─ Enters the if directly
   └─ Calls vtable+0x28 → sub_140111190 (Ring Buffer Read)
       └─ NOT AnimateAll!

5. The socket stays in state 3 FOREVER
   └─ Nobody calls select() to detect writable
   └─ Nobody transitions state 3 → 6
   └─ PushReplayData sees state != [6,10] and sends nothing
```

---

## Comparison: Lobby/Web vs Replay

### Lobby/Web (WORKS)
```
Server::Process
  └─ sub_1400e0de0(time, SocksClient)
      └─ SockAccept (accepts new connections)
      └─ SockAnimateAll ← sub_1400929A0
          └─ select() on the sockets
          └─ Detects writable → state 3→6 ✓
          └─ Reads data from connected sockets
```

### Replay (BROKEN)
```
Server::Process
  └─ sub_140110380(ReplayUploadManager, time, flag)
      └─ Every 5000ms: vtable+0x10 (Reconnect)
          └─ Only reconnects if state == 0
      └─ Every frame: vtable+0x28 (Ring Buffer Read)
          └─ Reads replay data from the buffer
          └─ It does NOT call AnimateAll!
          └─ The socket never advances from state 3!
```

---

## The Socket's State Machine

```
State 0 → idle/disconnected
State 1 → connecting (DNS resolved)
State 2 → listening (server socket)
State 3 → CONNECTING (WSAEWOULDBLOCK) ← YOU STAY HERE
State 4 → needs DNS resolve
State 5 → reserved
State 6 → CONNECTED (ready for data) ← YOU NEED TO REACH HERE
State 7 → waiting to send
State 8 → TLS shutdown
```

The 3→6 transition **only** happens inside `AnimateAll` (`sub_1400929A0:1526`):
```c
// sub_1400929A0.c:1524-1529
if ( _WSAFDIsSet(*(__int64 *)(v124 + 248), &writefds) > 0 )
{
    if ( *(__int16 *)(v124 + 56) == 3 )
    {
        sub_1400983B0(v120, v124);  // ← 3→6 transition
    }
}
```

---

## Diagnosis

### Look for these in the logs:

| Log | Meaning |
|-----|-------------|
| `"Socks::socksConnect: ... connect deferred (in progress)"` | State 3 reached, waiting for writable |
| `"connect exceeded timeout"` | AnimateAll was called but the timeout expired (10s) |
| `"DBG - Socks::Connect: ... connect succeeded"` | State 6 — connected! (debug level only) |
| `"Disconnected From Replay Server, Reconnecting..."` | State 0 — trying to reconnect |
| **NOTHING after "deferred"** | **AnimateAll is never called** ← ROOT CAUSE |
| `"WorldwideReplayUpload::PushReplayData - SockSend httpHeader has fatal error"` | Failed send attempt (state is not 6-10) |

### Check on the system:

```bash
# Is anything listening on the replay port?
netstat -an | findstr 8443

# Is the port open?
telnet 127.0.0.1 8443
```

---

## Solution

### Option A: Modify ReplayUploadManager::Process

In `sub_140110380.c`, the call to `vtable+0x28` should be `AnimateAll`:

```c
// Current (BUGGY):
(*(void (__fastcall **)(__int64, __int64))(*(__int64 *)v5 + 0x28))(v5, a2);
// vtable+0x28 = ring buffer read ← does NOT advance the states

// It should be something like:
sub_1400929A0(v5 + 0x30, a2);  // AnimateAll on the socket
// or modify vtable+0x28 so that it is AnimateAll
```

### Option B: Modify WorldwideReplayUpload's vtable+0x28

Change the `+0x28` vtable entry to point to `sub_1400929A0` (AnimateAll) instead of `sub_140111190` (ring buffer read). The ring buffer read would then have to be called separately.

### Option C: Add an AnimateAll call in vmethod_10 (Reconnect)

Make `sub_140110b90` (Reconnect) also call `AnimateAll` after attempting to connect:

```c
// sub_140110b90.c — after Connect:
if ( *(__int16 *)(a1[11] + 56LL) == 3 ) {
    // State 3 — call AnimateAll to advance
    sub_1400929A0(a1 + 0x30, timeout);
}
```

### Considerations

1. Option C is the least invasive — it only modifies Reconnect
2. Option A is the architecturally most correct
3. None of them requires modifying the `0x2a4` flag (it is already 1)
4. For the replays to be sent, you also need:
   - Clients connected over WebSocket
   - Active matches with peers
   - A reachable replayHost

---

## Visual Summary

```
Server::Process (sub_1400d81e0)
  ├─ sub_1400e0de0(Server[0x180])  → Lobby SocksClient
  │    └─ SockAnimateAll ✓          → select() → state 3→6 ✓
  ├─ sub_1400e0de0(Server[0x188])  → Web SocksClient
  │    └─ SockAnimateAll ✓          → select() → state 3→6 ✓
  └─ sub_140110380(Server[0x170])   → ReplayUploadManager
       ├─ Every 5000ms: vtable+0x10 (Reconnect)
       │    └─ Only if state == 0 → Connect → state 3
       └─ Every frame: vtable+0x28 (Ring Buffer Read)
            └─ It is NOT AnimateAll! ✗
            └─ The socket NEVER advances from state 3

                                     connect() → WSAEWOULDBLOCK → state=3 → "deferred"
                                         │
                                         ▼
                                    Is it AnimateAll()?
                                         │
                                       ┌─┴──┐
                                     Yes│  No│ → stays in state 3 FOREVER
                                       │    │  (because vtable+0x28 = ring buffer read)
                                       ▼    ▼
                                  select() → writable → state=6 ✓
```

---

## Code References

| Function | File | Purpose |
|---------|--------|-----------|
| `sub_1400D6790` | Server.cpp | Server constructor — initialises 0x2a4 = 1 |
| `sub_1400D81E0` | Server.cpp | Server::Process — calls sub_140110380 |
| `sub_140110380` | ReplayUploadManager.cpp | Process — **does NOT call AnimateAll** |
| `sub_1400E0DE0` | Server.cpp | processSocket — SockAccept + SockAnimateAll (lobby/web) |
| `sub_1400929A0` | sock.cpp | AnimateAll (select, advances the states) — **NEVER called for the replay** |
| `sub_140110B90` | WorldWideReplayUpload.cpp | vtable+0x10 — Reconnect callback |
| `sub_140110F10` | WorldWideReplayUpload.cpp | vtable+0x18 — Connect (starts the connection) |
| `sub_140111190` | WorldWideReplayUpload.cpp | vtable+0x28 — Ring Buffer Read (NOT AnimateAll) |
| `sub_140110F80` | WorldWideReplayUpload.cpp | vtable+0x30 — PushReplayData |
| `sub_1400983B0` | sock.cpp | SockConnectSucceeded — the 3→6 state transition |
| `sub_140091050` | sock.cpp | Socks::Connect — establishes the initial connection |
| `sub_1400987A0` | sock.cpp | socksConnect — runs the real connect() |

---
