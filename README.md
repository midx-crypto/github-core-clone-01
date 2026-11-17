# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/midx-crypto/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/midx-crypto/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge for the make.fun `github-core-clone-01` bounty, built as a statically linked Go binary plus a thin Node launcher. It serves git smart HTTP (upload-pack v2, receive-pack v0), the REST v3 API under `/api/v3`, the GraphQL v4 API at `/api/graphql`, and an MCP JSON-RPC 2.0 endpoint at `/mcp`, all from one in-memory store with deterministic ids and timestamps.

The Go forge binds an ephemeral port, registers it with the trial environment, and serves until the environment reports done. The Node launcher (`main.ts`) only spawns `./forge` in the same directory with the inherited environment, waits for it to exit, and exits with its exit code.

## Run it

```sh
node main.ts
```

Build the binary first (Go 1.27, stdlib only, no network):

```sh
export PATH=$HOME/.local/go/bin:$PATH
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -buildvcs=false -o forge .
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=g5h40b4t&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
