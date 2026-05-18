# Installation

Enju ships as a single binary (`enju`) that runs on Linux, macOS, and Windows.

## Prerequisites

- **Git 2.38+** — Enju uses `git merge-tree` internally; older versions lack the required flags.
- **Git must be on your PATH** — verify with `git --version`.

No other runtime dependencies. The coordinator, MCP server, and agent daemon are all inside the same binary.

## Download a pre-built binary

Pre-built binaries are attached to every [GitHub release](https://github.com/enju-ai/enju/releases). Download the archive for your platform:

| Platform | File |
|---|---|
| Linux x86-64 | `enju-linux-amd64.tar.gz` |
| Linux ARM64 | `enju-linux-arm64.tar.gz` |
| macOS x86-64 | `enju-darwin-amd64.tar.gz` |
| macOS Apple Silicon | `enju-darwin-arm64.tar.gz` |
| Windows x86-64 | `enju-windows-amd64.zip` |
| Windows ARM64 | `enju-windows-arm64.zip` |

Extract and place the `enju` binary somewhere on your PATH, for example `/usr/local/bin` on Linux/macOS:

```sh
tar -xzf enju-linux-amd64.tar.gz
sudo mv enju /usr/local/bin/
```

On Windows, extract the zip and add the folder containing `enju.exe` to your `PATH` environment variable.

## Build from source

Requires Go 1.26+.

```sh
git clone https://github.com/enju-ai/enju.git
cd enju
./build.sh build
sudo mv enju /usr/local/bin/
```

Or with plain `go build`:

```sh
go build -o enju ./cmd/enju/
```

## Verify

```sh
enju --version
```

You should see something like:

```
enju v0.1.0 (commit abc1234, built 2026-05-17)
```

## Next steps

- [Quickstart](quickstart.md) — run your first workflow in under five minutes.

---

> **TODO:** Add a one-line install script (`curl | sh`) that detects platform, downloads the right binary, and places it on PATH.
