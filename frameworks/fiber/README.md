# fiber

Fiber web framework on fasthttp, with prefork for multi-core scaling.

## Stack

- **Language:** Go 1.26
- **Framework:** Fiber 3
- **Build:** `golang:1.26-alpine` -> `alpine:3.23` runtime

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/pipeline` | GET | Returns `ok` (plain text) |
| `/baseline11` | GET | Sums the `a` and `b` query parameters |
| `/baseline11` | POST | Sums the query parameters + request body |
| `/json/{count}?m=N` | GET | First `count` dataset items with `total = price * quantity * m` |
| `/echo` | POST | Returns the request body back verbatim |
| `/delay/{ms}` | GET | Answers with `ms` after waiting that long |
| `/static/{file}` | GET | Serves `/data/static`, pre-compressed variant where the client takes one |

## Notes

- Routing and path/query access through the Fiber API: `fiber.Params[int]` and
  `fiber.Query[int]` read the two operands `/baseline11` is given by name, so the
  hot path does not build a map of the query string per request
- JSON through `c.JSON`, serialized per request
- Compression through the Fiber `compress` middleware, mounted on `/json` and `/static` rather
  than globally: on the few-byte bodies of `/pipeline` and `/baseline11` it can only add a `Vary`
  header, and those are the endpoints the throughput and CPU-per-request profiles drive
- Body limit raised to 25 MB so the in-out profile is not rejected
- A worker signalled directly drains through `ListenConfig.GracefulContext` rather than being
  cut off mid-response. `docker stop` signals PID 1 — the prefork master — which holds no
  handler of its own on purpose, so it exits and the workers follow it out within one 500 ms
  parent-pid poll

## Added profiles

`static-tls`, `json-tls`, `async` and `async-db`. The `/crud/*` routes are still served but no
profile drives them any more; they are kept because the multi-endpoint stacks are built on that
shape.

- `json-tls` and `static-tls` listen on `8081` when `/certs/server.crt` and `/certs/server.key`
  are mounted; it is the same router behind TLS, not a second copy of the handlers.
- Static file bodies are read from disk on every request, which the static profiles require in
  every mode — nothing here holds a copy, so a file replaced on disk is served from the next
  request onwards. Fiber's own static middleware cannot do that job here: its cache holds open
  file handles and never re-stats them, so a replaced file keeps being served for up to
  `CacheDuration`, and its `Compress` option writes `.fiber.br` twins into a directory the
  harness mounts read-only.
- Where the client accepts an encoding, the `.br`/`.gz` twin the harness ships beside the file is
  read instead of the original, chosen with Fiber's own `AcceptsEncodings` negotiation. The
  profile allows selecting it off `Accept-Encoding` where a framework has no API of its own for
  it, and the alternative is not compression but 842 KB of the 20-file rotation going out
  uncompressed, because fasthttp's Accept-Encoding matcher compares whole tokens and the profile
  sends `br;q=1, gzip;q=0.8`.
- `/delay/{ms}` parks the handler's goroutine on a timer, which is what makes the `async`
  profile a question about the process rather than about the handler.
- Postgres goes through `pgx`. Every database call carries a deadline: Fiber's `Ctx.Context()`
  is a background context and fasthttp has no per-request cancellation to put in it, so nothing
  else would bound a query whose client has gone away.
- `crud` runs cache-aside on Redis with a 200ms TTL and an explicit delete on update.
- `tags` is a JSONB column, so it comes back as bytes rather than a Go slice.

## Prefork

`app.Listen(":8080", fiber.ListenConfig{EnablePrefork: true})` hands off to
fasthttp's prefork manager, which is Fiber's answer to the same problem Node's
`cluster` module solves for the `express`, `fastify` and `koa` entries and
`SO_REUSEPORT` forking solves for `aiohttp`: one worker per core, which is the
worker count the implementation rules permit an entry to match.

- The master process binds nothing. It re-executes the binary `GOMAXPROCS` times
  with `FASTHTTP_PREFORK_CHILD=1` set and then supervises, restarting a child
  that dies. Children notice a dead master by their parent pid changing and exit.
- Each child calls `reuseport.Listen` for itself and sets `GOMAXPROCS(1)`, so the
  container runs one Go runtime per core rather than one runtime scheduling every
  core. The kernel spreads accepted connections across the listening sockets.
- The TLS listener on `8081` is opened the same way, once per child. In a child
  `EnablePrefork` means "take the `SO_REUSEPORT` socket for this address", not
  "fork again", and without it every child would race for an ordinary bind and
  all but one would lose it — silently, before the error was logged.
- The dataset, the Postgres pool and the Redis client are loaded only where
  `fiber.IsChild()` is true. The master has no use for them, and a pool opened
  before the fork would hand the same connections to every child.
- Anything the container holds once is divided by the child count. The Postgres
  pool is the one that matters: `DATABASE_MAX_CONN` is a budget for the
  container, not for a process, and a pool sized for one process would have the
  fleet asking for sixty-four times what the server will hand out.

What it costs is one Go runtime per core — N heaps, N garbage collectors, N sets
of background threads — paid for whether or not requests are arriving, which is
what the two fixed-rate profiles measure. `/benchmark -f fiber` reports the
deltas against this entry's results on `main`, so that trade is a number rather
than an argument.
