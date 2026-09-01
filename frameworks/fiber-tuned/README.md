# fiber-tuned

The [`fiber`](../fiber/) entry with the three changes standard mode does not
allow and tuned mode does. The server is the same: the routes, prefork, the
per-request static reads, the `.br`/`.gz` twin selection, the TLS listener on
`8081`, and the drain-on-signal shutdown are all `fiber`'s, unchanged.
`fiber`'s README documents them, and this file covers only what differs.

Both entries appear on the board — `fiber` as `mode: standard`, this one as
`mode: tuned` with the ring the board draws for tuned entries. `/benchmark`
runs them side by side, so each tuned change shows up as a delta against the
standard entry rather than as a claim.

## What is tuned

| | `fiber` (standard) | `fiber-tuned` |
|---|---|---|
| JSON | `encoding/json` | `sonic` behind `c.JSON` |
| Compress level | framework default | best speed |
| Postgres pool | opened lazily | opened to full size at startup |

Everything else — the body limit, the buffer sizes, `GOGC`, the worker count —
is left at the framework's and the runtime's defaults, the same as the standard
entry. Tuned mode would allow more; these three are the changes with a clear
rationale and a measurable effect, and the point of the pair is to measure them
rather than to collect knobs.

### JSON: sonic

`fiber.Config{JSONEncoder: sonic.Marshal, JSONDecoder: sonic.Unmarshal}`, and
the handlers that (de)serialize directly call `sonic` too. `c.JSON` runs the
configured encoder (`res.go` calls `app.config.JSONEncoder`), so this reaches
`/json`, `/async-db` and the crud responses without touching a handler.

- Tuned mode names this first: "Alternative JSON serializers (simd-json,
  sonic-json, etc.)", and the `json-comp` and `json-tls` tuned rules both lead
  with "alternative JSON libraries". It is the one change standard mode most
  squarely forbids and tuned mode most squarely invites.
- It is the JIT build, not the `encoding/json` fallback. sonic's build tags
  select the assembly path for `amd64` and `arm64` on every Go from 1.17 up to
  (not including) 1.28; the 1.26 toolchain in the Dockerfile is inside that
  range. On another architecture sonic compiles to a wrapper over
  `encoding/json` and this entry would simply match the standard one.
- The bytes are the same. For the dataset's types sonic's output is identical
  to `encoding/json` (checked), and the one default that differs — sonic does
  not HTML-escape `<`, `>`, `&` — has nothing to act on: the dataset contains
  none of those three characters in any string field.
- sonic compiles an encoder per type on first use. `pretouchJSON` does that at
  startup, once per worker, so the JIT compile lands where nothing is being
  measured instead of in the first requests of a run.

### Compression: best speed

`compress.New(compress.Config{Level: compress.LevelBestSpeed})` on the same two
prefixes the standard entry mounts it on (`/json`, `/static`).

- Tuned mode allows "tuned compression libraries" and "Any compression approach
  for static files"; `json-comp` tuned allows "tuned compression libraries … as
  long as the output is valid gzip or brotli".
- The profile it changes is `json-comp`, which scores requests per second for a
  body serialized and compressed per request and requires only *valid* gzip or
  brotli, not a ratio. Fiber's middleware picks brotli for the profile's
  `Accept-Encoding: gzip, br`; best speed is brotli at its fastest level (and
  gzip level 1 for a gzip-only client) rather than the middleware's default.
- Static is unaffected: the twins on disk are already compressed, and the
  middleware leaves an encoded body alone — so the level only ever applies to
  the per-request `/json` path.

### Postgres pool: eager fill

`cfg.MinConns = cfg.MaxConns`, alongside the `MaxConns` the standard entry
already sets from `DATABASE_MAX_CONN`.

- Tuned mode allows "custom pool sizes … or driver-specific optimizations
  beyond defaults". The *size* is unchanged from the standard entry — it is the
  server's budget divided by the worker count, and a pool bigger than Postgres
  accepts only fails to fill — so what is tuned is *when* the connections open.
- pgxpool opens connections lazily by default, so in the standard entry the
  first `async-db` requests of a run each pay for a TCP connect and a Postgres
  handshake. `MinConns` set to the pool size makes pgxpool open them at startup,
  in a background goroutine, before load arrives.

## Build

Identical to `fiber` (`golang:1.26-alpine` → `alpine:3.23`, `CGO_ENABLED=0`).
sonic needs no cgo. The only dependency `fiber` does not have is
`github.com/bytedance/sonic`.
