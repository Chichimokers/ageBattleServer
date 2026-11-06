# ageBattleServer

`ageBattleServer` is a Go relay server for LAN and offline game sessions. It
can be used as a component of [ageLANServer](https://github.com/luskaner/ageLANServer)
or embedded in another Go application. The repository contains a command-line
server and a Go API for applications that need to start and manage the server
in-process.

This project is not affiliated with, sponsored by, or endorsed by Microsoft or
any game publisher. Product names are used only to describe interoperability.

## Features

- TCP game connections
- WebSocket game connections, with optional TLS
- Optional out-of-band WebSocket endpoint
- Optional LAN presence broadcasts over UDP
- Session and observer management
- Configurable logging and resource limits

## Requirements

- Go 1.27 or newer
- A network environment in which the configured listener ports are available

## Install

Build the command-line server from the repository root:

```text
go install github.com/luskaner/ageBattleServer/cmd/battleserver@latest
```

For local builds and configuration details, see
[`DEVELOPMENT.md`](DEVELOPMENT.md).

## Run

```powershell
.\battleserver.exe -help
```

The server accepts command-line options for listener ports, TLS certificate
paths, session limits, observer settings, and logging. Run `-help` for the complete
list. Keep private keys and passwords outside the repository; environment-specific
configuration should not be committed.

## Use as a Go package

The package exposes a configurable server for applications that prefer an
in-process integration:

```go
import (
    "context"

    "github.com/luskaner/ageBattleServer"
)

func run(ctx context.Context) error {
    cfg := battleserver.DefaultConfig()
    cfg.BsPort = 27012

    srv, err := battleserver.New(cfg)
    if err != nil {
        return err
    }

    return srv.Run(ctx)
}
```

See the package documentation in [`battleserver.go`](battleserver.go) for the
available configuration and lifecycle methods.

## Project layout

- `battleserver.go` - public embeddable API.
- `cmd/` - command-line entry points and development tools.
- `internal/` - implementation details and test support.

Development requirements, testing commands, and contribution guidance are in
[`DEVELOPMENT.md`](DEVELOPMENT.md).

## Scope and distribution

This repository is a software project, not an official game server release.
Before sharing a copy, review every included source file, fixture, generated
file, and dependency for the rights needed for that distribution. Do not add
proprietary binaries, decompilations, credentials, private keys, or recorded
traffic to a public release.

This project is distributed under the
[GNU Affero General Public License v3.0](LICENSE).
