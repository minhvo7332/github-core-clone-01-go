# github-core-clone-01-go

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/minhvo7332/github-core-clone-01-go/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/minhvo7332/github-core-clone-01-go/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

GitForge v3: a GitHub-compatible git forge in a single process, with a static
Go core behind a thin Node launcher. Smart-HTTP git (upload-pack v2 /
receive-pack v0), REST v3, GraphQL and MCP, over one in-memory object store.
No dependencies at run time: the core is a statically linked Linux amd64
binary (`forge`, CGO_ENABLED=0) that the Node entrypoint (`main.ts`) spawns
and registers with the harness.

What it implements:

- **Git smart HTTP** at `/{owner}/{repo}.git` — `upload-pack` (protocol v2
  when the client asks, v0 otherwise) and `receive-pack` (v0), including
  shallow clones, deepen, annotated tags, and non-fast-forward rejection.
- **REST v3** under `/api/v3` — repositories, branches, commits, contents
  (create/update/delete with SHA checks), issues, labels, comments, pull
  requests (files, merge with squash, conflict detection), and GitHub-style
  pagination with `Link` headers.
- **GraphQL** at `/api/graphql` — repositories, issues, pull requests,
  commits, files, and mutations (`createIssue`, `createPullRequest`,
  `mergePullRequest`, `addComment`) with stable opaque global ids.
- **MCP** at `/mcp` — JSON-RPC 2.0 with `server/discover`, `initialize`,
  `tools/list`, and `tools/call` for the repository, issue, and pull-request
  tools.

## Why Go

The Node forge's hot paths are thousands of small sequential requests plus
50-way concurrent clones. The Go core answers them with a hand-rolled raw-TCP
HTTP/1.1 server (one parse, one socket write per request), zero-alloc hot
paths (pre-serialized issue JSON, pooled zlib buffers, cached packs and
depth-1 packs), and goroutines for the concurrent clone workload — no
per-request stream machinery, no garbage-collector pauses on the timed path.

## Run it

```sh
node main.ts
```

Requires Node 22+ (uses type stripping — no build step). The server listens
on an ephemeral port, registers it with `$MAKEFUN_ENV_URL/register`, and
exits when `$MAKEFUN_ENV_URL/done` reports done.

To rebuild the core:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=vtp8qmbj&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
