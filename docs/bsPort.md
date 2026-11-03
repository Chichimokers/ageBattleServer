# bsPort — Documentation

## General Description

The `bsPort` port (`-bsPort`) is the **server's main port** for incoming game connections. It is the TCP port where clients connect directly to play.

---

## Configuration Parameter

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `-bsPort` | uint | `27012` (0x6984) | TCP port for incoming game connections |

### Program Help

```
-bsPort                Override the port advertised for incoming connections
```

---

## Protocol

### TCP (Transmission Control Protocol)

```c
// FUN_140087d80 - Creation of the main TCP socket
socket(2, 1, 6);
// AF_INET=2, SOCK_STREAM=1, IPPROTO_TCP=6
```

It is the **main game port**. All client connections for playing go through this port.

---

## Initialising the bsPort

### Argument Parsing (FUN_1400d1310)

```c
// FUN_1400d1310.c, line 548:
iVar11 = _stricmp(pcVar22, "bsPort");
if (iVar11 == 0) {
    if (*_Str != '\0') {
        iVar11 = atoi(_Str);
        // Stores into the low word of local_348
        local_348 = CONCAT22(local_348._2_2_, (short)iVar11);
    }
}
```

### TCP Socket Initialisation (FUN_1400d8b70)

```c
// FUN_1400d8b70 - Initialisation of the main TCP socket
void InitializeRawSocket(Server *server, short *params) {
    // 1. Create SocketManager
    SocketManager *sm = new SocketManager(0, 0);  // TCP
    server->socketManager = sm;
    
    // 2. Create TCP socket
    void *socket = sm->CreateSocket(0);  // 0 = TCP
    server->listenSocket = socket;       // Store at Server+0x190
    
    // 3. Bind to the bsPort
    //    *param_2 = params[0] = bsPort
    sm->Bind(socket, *params);
    
    // 4. Listen with backlog 1000
    sm->Listen(socket, 1000);
}
```

### Decompiled Code (FUN_1400d8b70)

```c
void FUN_1400d8b70(longlong param_1, undefined2 *param_2) {
    // Create TCP SocketManager
    plVar3 = (longlong *)FUN_14009be00(0, 0);  // SocketManager(0, 0)
    *(longlong **)(param_1 + 0x180) = plVar3;    // Server+0x180 = socketManager
    
    // Create TCP socket
    uVar4 = (**(code **)(*plVar3 + 8))(plVar3, 0);  // CreateSocket(0) = TCP
    *(undefined8 *)(param_1 + 0x190) = uVar4;          // Server+0x190 = listenSocket
    
    // Bind to the bsPort (param_2[0])
    cVar2 = (**(code **)(**(longlong **)(param_1 + 0x180) + 0x38))(
        *(longlong **)(param_1 + 0x180), uVar4, *param_2
    );
    if (cVar2 == '\0') {
        // Error: "raw SockBind port %u failed"
        _CxxThrowException(...);
    }
    
    // Listen with backlog 1000
    cVar2 = (**(code **)(**(longlong **)(param_1 + 0x180) + 0x40))(
        *(longlong **)(param_1 + 0x180), 
        *(undefined8 *)(param_1 + 0x190), 
        1000
    );
    if (cVar2 == '\0') {
        // Error: "raw SockListen port %u failed"
        _CxxThrowException(...);
    }
}
```

### Storage in Server (FUN_1400d6790)

```c
// FUN_1400d6790.c, line 76:
// publicPort is stored at Server+0x232
*(short *)(param_1 + 0x232) = param_3[0x8c];  // bsPort -> publicPort
```

**Note**: `bsPort` is stored as `publicPort` at Server+0x232, because it is the public port that is advertised to clients.

---

## Connection Handling

### Accepting Connections (FUN_1400e0de0)

```c
// FUN_1400e0de0 - Accepts new TCP connections
void Server::ProcessSocket(
    longlong this,
    SocketManager *sm,
    void *listenSocket,
    ClientList *clients
) {
    // 1. Accept a new connection
    void *newSocket;
    int result = sm->Accept(sm, listenSocket, &newSocket);
    
    while (result == SAR_Success) {
        // 2. Create a new client
        Client *client = new Client(sm);
        client->socket = newSocket;
        client->parentSession = this;
        
        // 3. Add to the client list
        clients->push_back(client);
        
        // 4. Update the TcpConnections metric
        UpdateMetric(TcpConnections, clients->size());
        
        // 5. Check the timeout (max 10ms per cycle)
        if (GetElapsedTime() > 10) {
            break;  // Exit if it takes too long
        }
        
        // 6. Accept the next connection
        result = sm->Accept(sm, listenSocket, &newSocket);
    }
    
    // 7. Process existing sockets (AnimateAll)
    sm->SockAnimateAll(sm);
}
```

### Decompiled Code (FUN_1400e0de0)

```c
void FUN_1400e0de0(undefined8 param_1, longlong *param_2, undefined8 *param_3, longlong *param_4) {
    int result;
    void *newSocket;
    longlong startTime;
    
    // Start the timing measurement
    startTime = GetPerformanceCounter();
    
    // Accept a new connection
    // SockAccept(sm, listenSocket, &newSocket)
    result = (**(code **)(*param_2 + 0x48))(param_2, *param_3, &local_res10);
    
    while (1) {
        if (result != 0) {
            // Error, or there are no more connections
            if (result == 2) {
                // SAR_ListenError - Fatal error
                _CxxThrowException("Server listening socket failed.");
            }
            break;
        }
        
        // Create the client
        local_80 = FUN_14022c4c0(0xa8);  // Size: 168 bytes
        if (local_80 != 0) {
            // Client constructor
            lVar4 = FUN_1400d4770(local_80, param_2);
        }
        *(undefined8 *)(lVar4 + 0x18) = local_res10;  // socket
        *(undefined8 *)(lVar4 + 0x38) = param_1;       // parentSession
        
        // Add to the client list
        plVar1 = (longlong *)param_4[1];
        if (plVar1 == (longlong *)param_4[2]) {
            // Vector full, resize
            FUN_14007fbc0(param_4, plVar1, &local_88);
        } else {
            *plVar1 = lVar4;
            param_4[1] = param_4[1] + 8;
        }
        
        // Update the metric
        uVar5 = FUN_1400809c0();
        FUN_1400806e0(uVar5, 0, param_4[1] - *param_4 >> 3);
        
        // Check the timeout (10ms)
        lVar4 = FUN_14010f310();
        if (10 < (ulonglong)(lVar4 - startTime)) {
            result = 1;  // SAR_Timeout
            break;
        }
        
        // Accept the next one
        result = (**(code **)(*param_2 + 0x48))(param_2, *param_3, &local_res10);
    }
    
    // Process existing sockets (read/write)
    // SockAnimateAll(sm)
    (**(code **)(*param_2 + 0xa8))(param_2);
}
```

---

## Client Packet Processing

### Main Loop (FUN_1400d8e10)

```c
// FUN_1400d8e10 - Processes packets of connected clients
void Server::ProcessSocks(longlong this, longlong time) {
    // Iterate over the client list
    for (auto& client : this->clients) {
        // Check whether the client needs processing
        if (!client->observerMode || client->CanProcess()) {
            int bytesReceived = 0;
            int maxPackets = this->maxPacketsPerCycle;  // Server+0x2a8
            
            // Receive packets
            for (int i = 0; i < maxPackets; i++) {
                // Reset the buffer
                packet->Reset(0xFFFF);
                
                // Receive data
                ushort dataSize = packet->GetSize();
                void *data = packet->GetData(0);
                
                // SockRecvSome(sm, socket, data, size)
                bytesReceived = client->sm->RecvSome(
                    client->sm,
                    client->socket,
                    data,
                    dataSize
                );
                
                if (bytesReceived < 1) break;
                
                // Adjust the size
                packet->SetSize(bytesReceived);
                
                // Update the BytesReceived metric
                UpdateMetric(BytesReceived, bytesReceived);
                
                // Parse the network message
                // BinaryStream::ParseMessage(packet)
                (**(code **)(**(longlong **)(this + 0x168) + 0x18))();
                
                // Process the packet in the lobby SocksClient
                // FUN_1400de050(session, packet, peer)
                FUN_1400de050(client->session, packet, peerInfo);
            }
        }
    }
    
    // Process session replay data
    FUN_1400ea460(client->sessions, time, this);
}
```

---

## Client Structure

### When a Connection Is Created

```c
// Client structure (FUN_1400d4770)
struct Client {
    void *vftable;              // +0x00
    void *socket;               // +0x18 - TCP socket
    void *parentSession;        // +0x38 - Parent session
    void *socketManager;        // +0x20 - SocketManager
    longlong socketId;          // +0xF8 - Socket ID
    // ... more fields
};
```

### Storage in Server

```
Server+0x140 → ClientList (vector of Client pointers)
Server+0x148 → ClientListEnd
Server+0x150 → ClientListCapacity
```

---

## Game Protocol Messages

### Processed by bsPort

Once the TCP connection is established, the following messages are processed:

| Type ID | Message | Direction | Description |
|---------|---------|-----------|-------------|
| `0x09` | `GameDataMessage` | Client → Server | Game state data |
| `0x10` | `ProxyMessage` | Client → Server | Proxy between peers |
| `0x17` | `GameDataAndChecksumMessage` | Client → Server | Game data + checksum |
| `0x18` | `StartObserveRequestMessage` | Client → Server | Request to observe |
| `0x1A` | `ActivateObserverMessage` | Client → Server | Activate observer mode |
| `0x1B` | `StopObserveMessage` | Client → Server | Deactivate observer mode |
| `0x1D` | `SetObservableParametersMessage` | Client → Server | Configure observable parameters |
| `0x1F` | `LeaveSimulationMessage` | Client → Server | Leave the simulation |
| `0x13` | `LobbyRelayMessage` | Bidirectional | Relay via the lobby |
| `0x14` | `LobbyPingMessage` | Bidirectional | Lobby ping |
| `0x15` | `LobbyConnectMessage` | Server → Lobby | Initial lobby connection |

---

## Server Offsets

| Offset | Field | Type | Description |
|--------|-------|------|-------------|
| `+0x140` | ClientList | `vector` | List of connected clients |
| `+0x148` | ClientListEnd | `ptr` | End of the list |
| `+0x168` | LobbySocksClient | `ptr` | Lobby client |
| `+0x180` | SocketManager | `ptr` | TCP socket manager |
| `+0x190` | ListenSocket | `ptr` | Listening TCP socket |
| `+0x232` | publicPort | `ushort` | Public port (= bsPort) |
| `+0x2a8` | MaxPacketsPerCycle | `int` | Max packets per cycle |

---

## Lifecycle of a Connection

```
Client                                    Server (bsPort)
   │                                              │
   │──────── SYN ────────────────────────────────→│
   │                                              │
   │←──────── SYN-ACK ────────────────────────────│
   │                                              │
   │──────── ACK ────────────────────────────────→│  ← Connection established
   │                                              │
   │                                              │  FUN_1400e0de0 (Accept)
   │                                              │  └→ Creates a new Client
   │                                              │  └→ Adds to ClientList
   │                                              │
   │──────── GameDataMessage ────────────────────→│
   │                                              │  FUN_1400d8e10 (ProcessSocks)
   │                                              │  └→ RecvSome()
   │                                              │  └→ ParseMessage()
   │                                              │  └→ ProcessPacket()
   │                                              │
   │←──────── ADDPEER / REMOVEPEER ───────────────│
   │                                              │
   │──────── Heartbeat / Ping ───────────────────→│
   │←──────── Heartbeat / Ping ───────────────────│
   │                                              │
   │──────── DROPOUT ────────────────────────────→│
   │                                              │
   │──────── FIN ────────────────────────────────→│  ← Disconnection
   │                                              │
```

---

## Difference from Other Ports

| Port | Type | Purpose |
|--------|------|-----------|
| `-bsPort` | TCP | **Main game** — client connections |
| `-webSocketPort` | TCP+TLS | WebSocket connections (wss://) |
| `-lobbyPort` | TCP | Heartbeat with the lobby server |
| `-outOfBandPort` | TCP | OOB ping (OutOfBandServer) |
| `-relayBroadcastPort` | UDP | Presence broadcast |
| `-replayHost:replayPort` | TCP | Replay upload (not implemented here: no outbound connection is opened) |

---

## Configuration Example

```bash
BattleServer.exe ^
  -name "My Server" ^
  -bsPort 27012 ^
  -webSocketPort 9090 ^
  -lobbyHost lobby.example.com ^
  -lobbyPort 8443 ^
  -outOfBandPort 27013 ^
  -relayBroadcastPort 27014 ^
  -publicIP 203.0.113.50 ^
  -publicPort 27012
```

**Note**: `-publicPort` generally has to equal `-bsPort` so that clients can connect from outside the local network.

---

## Associated Metrics

| Metric | Description |
|---------|-------------|
| `TcpConnections` | Active TCP connections on bsPort |
| `TcpConnectionErrors` | TCP connection errors |
| `Clients` | Connected clients |
| `ValidClients` | Authenticated clients |
| `BytesReceived` | Total bytes received |
| `BytesSent` | Total bytes sent |
| `PacketsInUse` | Active packets |

---

## Involved Functions

| Function | Address | Purpose |
|---------|---------|---------|
| `FUN_1400d1310` | `0x1400d1310` | Parsing of the `-bsPort` argument |
| `FUN_1400d8b70` | `0x1400d8b70` | TCP socket initialisation |
| `FUN_1400d6790` | `0x1400d6790` | Stores bsPort as publicPort |
| `FUN_1400e0de0` | `0x1400e0de0` | Accepts new connections |
| `FUN_1400d8e10` | `0x1400d8e10` | Processes client packets |
| `FUN_1400de050` | `0x1400de050` | Processes protocol messages |
| `FUN_1400901a0` | `0x1400901a0` | SockAccept (accept a connection) |
| `FUN_14009be00` | `0x14009be00` | SocketManager constructor |
| `FUN_140087d80` | `0x140087d80` | TCP socket creation |

---

## Summary

| Characteristic | Detail |
|----------------|---------|
| **Protocol** | TCP (SOCK_STREAM, IPPROTO_TCP) |
| **Default** | 27012 (0x6984) |
| **Purpose** | Main game connections |
| **Socket backlog** | 1000 |
| **Max packets/cycle** | Configurable (Server+0x2a8) |
| **Stored as** | publicPort (Server+0x232) |

---

## See Also

- [oob.md](oob.md) — Out-of-Band ping service
- [relay.md](relay.md) — Presence broadcast
- [LOBBY-PROTOCOL.md](LOBBY-PROTOCOL.md) — Heartbeat system
- [replays.md](replays.md) — Replay system
- [README.md](README.md) — General project description
