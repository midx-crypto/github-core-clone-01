# github-core-clone-01

[![make.fun](https://make.fun/badge.svg)](https://make.fun/bounty/github-core-clone-01)

A standalone, GitHub-compatible git forge, built for [make.fun's "Build a GitHub-compatible git forge" bounty](https://make.fun/bounty/github-core-clone-01) ($500 in USDC).

Referral code: `g5h40b4t`

## What it serves

One process, Node 22 standard library only — no dependencies, no git binary, no network beyond the trial env:

| Surface | Route | What it does |
| --- | --- | --- |
| Health | `GET /healthz` | 200 `{"ok": true}` |
| REST v3 | `/api/v3/...` | Repositories, branches, commits, contents, issues, comments, labels, pull requests, files and merges |
| Git smart HTTP | `/{owner}/{repo}.git/...` | `info/refs`, `git-upload-pack` (protocol v2: `ls-refs`, `fetch`, shallow/deepen), `git-receive-pack` (v0, fast-forward only, per-ref report-status) |
| GraphQL v4 | `POST /api/graphql` | The documents `gh` and `github-mcp-server` send: viewer, repository, issues, pull requests, mutations, `node(id:)`, introspection, batched operations |
| MCP | `POST /mcp` | JSON-RPC 2.0 Streamable HTTP: `server/discover` (2026-07-28) and the legacy `initialize` handshake, `tools/list`, `tools/call` over the same store as REST |

Packfiles are built and parsed in-process (`node:zlib` inflate/deflate, `node:crypto` SHA-1), including OFS/REF delta resolution on push. The store is entirely in memory.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later. The lifecycle (`main.ts`) binds an ephemeral port with `listen(0)`, registers it at `$MAKEFUN_ENV_URL/register`, polls `$MAKEFUN_ENV_URL/done`, and exits 0 when the trial is over.

## Layout

- `main.ts` — the entry <-> env lifecycle (bind, register, wait for done).
- `app.ts` — the HTTP server and the REST v3 surface.
- `store.ts` — the in-memory store: git objects, refs, branch snapshots, issues, pull requests, labels.
- `git.ts` — packfiles, pkt-lines, protocol v2 upload-pack and v0 receive-pack.
- `graphql.ts` — the GraphQL parser and executor.
- `mcp.ts` — the MCP endpoint and its tools.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT).

## License

MIT. See `LICENSE`.
