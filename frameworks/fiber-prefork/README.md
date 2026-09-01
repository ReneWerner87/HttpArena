# fiber-prefork

Fiber 3 with prefork on: one child process per core, each accepting on its own
`SO_REUSEPORT` socket.

## What this entry is

The [`fiber`](../fiber/) entry's source, built with `FIBER_PREFORK=1`. There is
no code here — `build.sh` builds the sibling directory with the build argument
set — because the entry only means something if prefork is the one thing that
differs. Same handlers, same middleware, same Fiber version: whatever the two
rows differ by on the board is the process model.

## Why it is tuned rather than standard

Standard mode is the framework's default configuration, and the baseline profile
spells the boundary out: no worker count beyond framework defaults. Prefork is
off by default in Fiber and multiplies the process count by the core count, so
this side of the pair is `mode: tuned`.

## How it runs

`app.Listen(":8080", fiber.ListenConfig{EnablePrefork: true})` hands off to
fasthttp's prefork manager:

- The master process binds nothing. It re-executes the binary `GOMAXPROCS` times
  with `FASTHTTP_PREFORK_CHILD=1` set and then supervises, restarting a child
  that dies. Children notice a dead master by their parent pid changing and exit.
- Each child calls `reuseport.Listen` for itself and sets `GOMAXPROCS(1)`, so the
  fleet is one runtime per core rather than one runtime scheduling every core.
  The kernel spreads accepted connections across the listening sockets.
- The TLS listener on `8081` is opened the same way, once per child: in a child
  `EnablePrefork` means "take the `SO_REUSEPORT` socket for this address", not
  "fork again", and without it every child would race for an ordinary bind and
  all but one would lose it.
- Anything a single process would hold once is divided by the child count. The
  Postgres pool is the one that matters: `DATABASE_MAX_CONN` is a budget for the
  container, not for a process, and a pool sized for one process would have the
  fleet asking the server for sixty-four times what it will hand out.

## What to expect from it

Prefork is not free and this pair exists to price it rather than to assume it.
It removes cross-core coordination inside one Go runtime and gives each core its
own accept queue, which is where the throughput profiles can gain. What it adds
is one Go runtime per core: N heaps, N garbage collectors, N sets of background
threads, all of which are paid for whether or not requests are arriving — so the
two fixed-rate profiles, `latency-10k` above all, are where it can lose. It also
gives up shared process state, which this entry has none of.
