# bun-http2

`Bun.serve` with Bun's experimental `http2: true` flag, no framework on top. The HTTP/2 half of
the plain [`bun`](../bun) entry: same routes, same JSON shape, same static handler, one flag more.

## Stack

- **Language:** TypeScript
- **Runtime:** [Bun](https://github.com/oven-sh/bun) (needs > 1.4.0 — see *Base image* below)
- **Framework:** none, `Bun.serve` from the standard library
- **Build:** Single stage on `oven/bun:canary`

## Listeners

| Port | Transport | Protocol | Profiles |
|------|-----------|----------|----------|
| 8443 | TLS | h2 via ALPN, HTTP/1.1 for clients that don't ask | `baseline-h2`, `static-h2` |
| 8082 | cleartext | h2c on the HTTP/2 connection preface (prior knowledge) | `baseline-h2c`, `json-h2c` |

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/baseline2` | GET | Sums query parameter values |
| `/json/:count` | GET | Serializes a slice of the dataset, gzipped when the client accepts it |
| `/static/:file` | GET | Serves one of the 20 files from `/data/static`, read off disk per request |

## Base image

`http2: true` for `Bun.serve` landed after 1.4.0, so `oven/bun:1.4` would bind both ports and
answer HTTP/1.1 on them — the flag is ignored there rather than rejected, which validation catches
as *server responded with HTTP/1.1*. The Dockerfile pins `oven/bun:canary` through a
`BUN_IMAGE` build arg; move it back to `oven/bun:1.4` once 1.4.1 ships, with no change to
`server.ts`.

## Notes

- Nothing in the handlers is h2-aware. Bun picks the protocol per connection — ALPN over TLS, the
  connection preface in cleartext — and the same `fetch`/`routes` config serves both. That is what
  makes a number here readable against the plain `bun` entry as the cost of the protocol rather
  than of a second implementation.
- Scaling is not the cluster module: every process calls `Bun.serve` with `reusePort`, binds the
  same ports and lets the kernel spread the accepts. The process count comes from `nproc`, lowered
  to the cgroup quota when there is one.
- `Bun.serve` cannot refuse HTTP/1.1 on the cleartext port (`http1: false` requires `http3`), so
  :8082 answers an HTTP/1.1 request too. The h2c profiles drive it with `h2load -p h2c` and
  validation probes it with `--http2-prior-knowledge`, so neither can silently measure HTTP/1.1.
- `Bun.serve` does no content negotiation, so `/json` gzips its own body when `Accept-Encoding`
  asks for it. The `json-h2c` load generator does not ask, so that profile measures the plain path.
- `Bun.serve` has no pre-compressed static API either, so `/static` selects the `.br`/`.gz` sibling
  the harness leaves on disk off `Accept-Encoding`. Nothing is compressed at runtime and the
  handles stay lazy, so replacing a file on disk shows up on the next request.
