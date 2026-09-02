# bun

Bun's own HTTP server, `Bun.serve`, with no framework on top. One process per CPU, all sharing
every port through `reusePort`. The same routes serve HTTP/1.1, HTTP/2 and HTTP/3 - the protocol
is a flag on the listener, not a second implementation.

## Stack

- **Language:** TypeScript
- **Runtime:** [Bun](https://github.com/oven-sh/bun) 1.4.1
- **Framework:** none, `Bun.serve` from the standard library
- **Build:** Single stage on `oven/bun:${BUN_VERSION}`, `BUN_VERSION` a build arg defaulting to `1.4.1`

## Listeners

| Port | Transport | Protocol | Profiles |
|------|-----------|----------|----------|
| 8080 | TCP | HTTP/1.1 | `baseline`, `pipelined`, `limited-conn`, `json-comp`, `async*`, `latency-*`, `fortunes` |
| 8081 | TLS | HTTP/1.1 only, ALPN `http/1.1` | `json-tls`, `static-tls`, `8gbit` |
| 8082 | TCP | h2c on the HTTP/2 preface (prior knowledge) via `http2: true` | `baseline-h2c`, `json-h2c` |
| 8443/tcp | TLS | h2 via ALPN (`http2: true`), HTTP/1.1 with `Alt-Svc` for the rest | `baseline-h2`, `static-h2` |
| 8443/udp | QUIC | HTTP/3 via `http3: true`, Bun's own lsquic | `baseline-h3`, `static-h3` |

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/pipeline` | GET | Returns `ok` (plain text) |
| `/baseline11` | GET/POST | Sums query parameter values, plus the body for POST |
| `/baseline2` | GET | Sums query parameter values |
| `/json/:count` | GET | Serializes a slice of the dataset, gzipped when the client accepts it |
| `/echo` | POST | Returns the request body back verbatim |
| `/static/:file` | GET | Serves one of the 20 files from `/data/static`, read off disk per request |
| `/async-db` | GET | Postgres range query over `items`, `min`/`max`/`limit` |
| `/crud/items` | GET/POST | Paginated list by category; POST upserts |
| `/crud/items/:id` | GET/PUT | Cache-aside read through Redis; PUT updates and invalidates |
| `/fortunes` | GET | Postgres rows plus one runtime row, sorted, HTML-escaped into a table |

Both TLS listeners are only opened when the harness mounts `/certs`.

## Notes

- Postgres and Redis are `Bun.SQL` and `Bun.RedisClient`, both shipped with the runtime, so the
  database profiles cost this entry no dependency.

- Bun was on the board as a WebSocket echo server only. This entry is the plain HTTP floor of the
  runtime itself, so a Bun framework entry can be read against it.
- Scaling is not the cluster module: every process calls `Bun.serve` with `reusePort`, binds the
  same port and lets the kernel spread the accepts. The process count comes from `nproc`, lowered
  to the cgroup quota when there is one.
- Routing is a handful of string comparisons on the path taken out of `req.url`, since with no
  framework there is no router to measure.
- `Bun.serve` does no content negotiation, so `/json` gzips its own body when `Accept-Encoding`
  asks for it, and sends it uncompressed otherwise. The `json-h2c` load generator does not ask,
  so that profile measures the plain path.
- `http2: true` and `http3: true` are experimental in Bun. The h2 flag shipped in 1.4.1
  ([oven-sh/bun#40137](https://github.com/oven-sh/bun/pull/40137)); on 1.4.0 it is ignored rather
  than rejected and the listener answers HTTP/1.1, which validation reports as such. The h3 flag
  ([oven-sh/bun#29768](https://github.com/oven-sh/bun/pull/29768)) is in 1.4.0 already; 0-RTT is
  off and `server.upgrade()` returns false over h3, neither of which the profiles touch.
- :8081 deliberately has no `http2` flag: `json-tls`, `static-tls` and `8gbit` require the ALPN to
  settle on `http/1.1`, and with the flag Bun would offer h2 to a client that asks.
- Bun cannot refuse HTTP/1.1 on the cleartext h2c port (`http1: false` requires `http3`), so
  :8082 answers an HTTP/1.1 request too. The profiles drive it with `h2load -p h2c` and validation
  probes it with `--http2-prior-knowledge`, so neither can silently measure HTTP/1.1.
- Every worker binds udp/8443 through the same `reusePort`, and the kernel spreads QUIC connections
  by 4-tuple. That holds here because the load generator never migrates a connection mid-run.
- `/echo` counts the body chunk by chunk instead of buffering it, which keeps 20 MB requests on
  hundreds of connections out of memory.
