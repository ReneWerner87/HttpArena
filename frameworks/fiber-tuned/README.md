# fiber-tuned

The [`fiber`](../fiber/) entry with three settings the standard entry leaves at
the framework's defaults: sonic for JSON, the compress middleware at its
best-speed level, and a Postgres pool filled at startup. sonic is what tuned
mode exists for and standard mode forbids. The other two are documented options
of the middleware and the driver the standard entry already uses — `carter` and
`salvo` run their compression at level 1 in standard mode — and they sit here
rather than in `fiber` because that entry's line is *default configuration*,
and a level is a configuration.

The server is otherwise the same: the routes, prefork, the per-request static
reads, the `.br`/`.gz` twin selection, the TLS listener on `8081`, and the
drain-on-signal shutdown are all `fiber`'s, unchanged. `fiber`'s README
documents them, and this file covers only what differs.

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

## Measured

Not board numbers — those come from `/benchmark` on the reference hardware.
These are from a 4-vCPU sandbox: one prefork worker of each entry (child mode,
`GOMAXPROCS(1)`) pinned to one core, the load generator on two others, 16
keep-alive connections, two rounds in alternating order. The metric that matters
is CPU per request, read from `/proc`, because it does not depend on whether the
generator saturates the server.

| request | `fiber` | `fiber-tuned` | delta |
|---|---|---|---|
| `/json/50?m=6`, `Accept-Encoding: gzip, br` (json-comp) | 378–386 µs, 1490 B | 145–146 µs, 1940 B | −62 % CPU, +30 % bytes |
| `/json/50?m=6`, no encoding (json-tls, minus TLS) | 82–84 µs | 59–64 µs | −25 % CPU |
| `/baseline11?a=13&b=42` (control, no delta touches it) | 11.3–13.3 µs | 11.4–11.5 µs | noise |

The control row is what makes the other two readable: nothing in this entry
touches `/baseline11`, and it comes out equal. On the same box, the two deltas
in isolation (`go test -bench`, the exact 8397-byte body):

| work | `fiber` | `fiber-tuned` |
|---|---|---|
| JSON marshal, `encoding/json` → sonic | 35.1 µs | 12.2 µs |
| brotli, level 4 → 0 | 259 µs → 1490 B | 68 µs → 1940 B |
| gzip, level 6 → 1 (a gzip-only client) | 62 µs → 1519 B | 33 µs → 1722 B |

The sum of the isolated deltas matches the end-to-end change to within the
framework's fixed cost per request, so the two tables are describing the same
thing. What the first table also shows is where the standard entry's json-comp
cost sits: brotli at fasthttp's default level 4 is about two thirds of it, and
four times what gzip at its own default takes for a body two per cent larger.
That is the default, and the standard entry ships the default.

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
  `Accept-Encoding: gzip, br`. `LevelDefault` maps to brotli level 4 and gzip
  level 6 (fasthttp's `CompressBrotliDefaultCompression` and
  `CompressDefaultCompression`); `LevelBestSpeed` maps to brotli level 0 and
  gzip level 1. Measured above: 259 µs → 68 µs per body for brotli, at 1490 B →
  1940 B on the wire.
- The level is an option of the same middleware the standard entry mounts, not
  a different library, and `carter` and `salvo` set theirs to level 1 in
  standard mode. It could live in `fiber`; whether it should is a question
  about what that entry is for, and this pair keeps the answer measurable.
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
- Whether that reaches the board is doubtful, and this README should say so:
  each profile runs three times and the best run is kept, which discards a cold
  start by design. The fill matters to the first requests after a container
  starts — a real thing in production, where a deploy is a cold start under
  live traffic — not to a steady-state score. The same goes for `pretouchJSON`.

## Build

Identical to `fiber` (`golang:1.26-alpine` → `alpine:3.23`, `CGO_ENABLED=0`).
sonic needs no cgo. The only dependency `fiber` does not have is
`github.com/bytedance/sonic`.
