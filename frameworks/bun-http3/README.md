# bun-http3

`Bun.serve` with Bun's experimental `http3: true` flag, no framework on top. The QUIC half of the
plain [`bun`](../bun) entry: same routes, same static handler, one flag more.

## Stack

- **Language:** TypeScript
- **Runtime:** [Bun](https://github.com/oven-sh/bun) 1.4
- **Framework:** none, `Bun.serve` from the standard library
- **QUIC/HTTP3:** Bun's own, lsquic inside uWebSockets — no proxy in front
- **Build:** Single stage on `oven/bun:1.4`

## Listeners

| Port | Transport | Protocol | Profiles |
|------|-----------|----------|----------|
| 8443/udp | QUIC | HTTP/3 | `baseline-h3`, `static-h3` |
| 8443/tcp | TLS | HTTP/1.1, `Alt-Svc: h3=":8443"` | none — readiness only |

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/baseline2` | GET | Sums query parameter values |
| `/json/:count` | GET | Serializes a slice of the dataset, gzipped when the client accepts it |
| `/static/:file` | GET | Serves one of the 20 files from `/data/static`, read off disk per request |

## Notes

- `http3: true` next to `tls` is the whole of it: Bun opens the UDP listener on the same port
  number, keeps HTTP/1.1 on TCP, and puts `Alt-Svc` on the TCP responses so a browser upgrades
  itself. Nothing in the handlers is QUIC-aware.
- The TCP half is not measured by either profile. It stays on because the harness decides an entry
  has started by reaching it over TCP — there is no HTTP/3 client in the readiness path, so an
  h3-only listener reads as a server that never came up.
- Scaling is not the cluster module: every process calls `Bun.serve` with `reusePort` and binds
  udp/8443, and the kernel spreads connections across them by 4-tuple. That holds for QUIC here
  because the load generator does not migrate a connection to a new address mid-run. The process
  count comes from `nproc`, lowered to the cgroup quota when there is one.
- QUIC mandates TLS, so a missing `/certs` mount is fatal rather than silently degrading to a
  plaintext listener the profiles would never reach.
- Bun's HTTP/3 is experimental: 0-RTT resumption is off, `server.upgrade()` returns false over h3,
  and unix sockets skip the QUIC listener. None of those sit on the path these two profiles measure.
- `/static` selects the `.br`/`.gz` sibling the harness leaves on disk off `Accept-Encoding`;
  nothing is compressed at runtime and the `Bun.file` handles stay lazy, so replacing a file on
  disk shows up on the next request.
