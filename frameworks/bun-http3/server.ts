// Bun's own HTTP/3 server: Bun.serve with `http3: true`, no framework on top.
//
// One flag next to `tls` and Bun opens a QUIC listener on the same port number
// over UDP, keeps HTTP/1.1 on TCP, and puts an Alt-Svc header on the TCP
// responses so a browser upgrades itself. The routes, the JSON shape and the
// static handler below are the ones the plain `bun` entry runs over HTTP/1.1 -
// nothing here is QUIC-aware - so a number from this entry reads against that
// one as the cost of the transport rather than of a second implementation.
//
// The TCP half is not measured by either subscribed profile. It stays on
// because the harness reaches an entry over TCP to decide it has started:
// there is no HTTP/3 client in the readiness path, so an h3-only listener
// looks like a server that never came up.
//
// Support is experimental in Bun: 0-RTT resumption is off, server.upgrade()
// returns false over h3, and unix sockets skip the QUIC listener. None of
// those are on the path these two profiles measure.

// A missing dataset is not fatal, and neither profile here reads it - the
// handler is kept so /json answers rather than 404s when something probes it.
type Item = {
    id: number;
    name: string;
    category: string;
    price: number;
    quantity: number;
    active: boolean;
    tags: string[];
    rating: { score: number; count: number };
};

let dataset: Item[] = [];
try {
    dataset = await Bun.file(process.env.DATASET_PATH || "/data/dataset.json").json();
} catch {}

const TEXT = { "content-type": "text/plain", "server": "bun" };
const JSON_PLAIN = { "content-type": "application/json", "server": "bun" };
const JSON_GZIP = { "content-type": "application/json", "content-encoding": "gzip", "server": "bun" };

function sumQuery(query: string): number {
    let sum = 0;
    for (const pair of query.split("&")) {
        const eq = pair.indexOf("=");
        if (eq < 0) continue;
        const n = parseInt(pair.slice(eq + 1), 10);
        if (!Number.isNaN(n)) sum += n;
    }
    return sum;
}

function multiplier(query: string): number {
    for (const pair of query.split("&")) {
        if (pair.startsWith("m=")) {
            const n = parseInt(pair.slice(2), 10);
            if (!Number.isNaN(n)) return n;
        }
    }
    return 1;
}

function json(count_: string, query: string, req: Request): Response {
    let count = parseInt(count_, 10);
    if (!(count > 0)) count = 0;
    if (count > dataset.length) count = dataset.length;
    const m = multiplier(query);

    const items = new Array(count);
    for (let i = 0; i < count; i++) {
        const d = dataset[i]!;
        items[i] = {
            id: d.id, name: d.name, category: d.category,
            price: d.price, quantity: d.quantity, active: d.active,
            tags: d.tags, rating: d.rating,
            total: d.price * d.quantity * m,
        };
    }
    const body = JSON.stringify({ items, count });

    // Bun.serve does no content negotiation, so the body is compressed here and
    // only when the client asked for it: a Content-Encoding nobody accepted is
    // a validation failure.
    const accept = req.headers.get("accept-encoding");
    if (accept !== null && accept.includes("gzip")) {
        return new Response(Bun.gzipSync(body), { headers: JSON_GZIP });
    }
    return new Response(body, { headers: JSON_PLAIN });
}

// ── static ──────────────────────────────────────────────────────────────────
// Same handler as the plain entry, and the same reasoning behind it.
//
// Content-Type is mapped from an explicit table rather than left to Bun.file's
// sniffing: static-h3 checks the header on woff2 and webp among others.
//
// Bun.file() is a lazy handle - the bytes are read when the Response is
// streamed and nothing is retained between requests, so the cache follows the
// disk, which is what the profile requires.
//
// Bun.serve has no pre-compressed static API, so the .br/.gz variants the
// harness leaves on disk are selected here off Accept-Encoding. Nothing is
// compressed at runtime: those bytes already exist next to the original, and
// picking one is a read of a different path.
const MIME: Record<string, string> = {
    css: "text/css", js: "text/javascript", html: "text/html",
    woff2: "font/woff2", svg: "image/svg+xml", webp: "image/webp",
    json: "application/json",
};

// Brotli first: it is the smaller of the two and every client that sends br
// also sends gzip. A client asking for neither gets the original bytes.
const ENCODINGS: ReadonlyArray<readonly [string, string]> = [
    ["br", ".br"],
    ["gzip", ".gz"],
];

async function serveStatic(name: string, req: Request): Promise<Response> {
    // No traversal outside the mount, and no directory reads.
    if (name.length === 0 || name.includes("/") || name.includes("..")) {
        return new Response("Not Found", { status: 404, headers: TEXT });
    }
    const base = "/data/static/" + name;
    const file = Bun.file(base);
    if (!(await file.exists())) {
        return new Response("Not Found", { status: 404, headers: TEXT });
    }
    const dot = name.lastIndexOf(".");
    const type = (dot > 0 && MIME[name.slice(dot + 1)]) || "application/octet-stream";

    // Content-Type stays that of the original file; only the encoding differs.
    const accept = req.headers.get("accept-encoding") ?? "";
    for (const [token, suffix] of ENCODINGS) {
        if (!accept.includes(token)) continue;
        const encoded = Bun.file(base + suffix);
        if (await encoded.exists()) {
            // Only the two headers the response is wrong without: Bun infers
            // Content-Type from the .br/.gz suffix and gets it wrong, and it
            // never sets Content-Encoding. Vary and Server are left off because
            // the profile scores bandwidth and they are ~38 bytes a response.
            return new Response(encoded, {
                headers: { "content-type": type, "content-encoding": token },
            });
        }
    }

    return new Response(file, { headers: { "content-type": type } });
}

// The query string, which is the one part of the request the router does not
// hand over.
const qs = (req: Request): string => {
    const q = req.url.indexOf("?");
    return q < 0 ? "" : req.url.slice(q + 1);
};

const routes = {
    "/baseline2": (req: Request) => new Response(String(sumQuery(qs(req))), { headers: TEXT }),

    "/json/:count": (req: any) => json(req.params.count, qs(req), req),

    // one segment, so a path with a slash in it never reaches the handler
    "/static/:name": (req: any) => serveStatic(req.params.name, req),
};

// QUIC needs TLS, so without the mounted pair there is no listener to open at
// all. Failing loudly beats binding an HTTP/1.1 port the profiles never touch
// and letting the run time out on a readiness probe instead.
const tlsKey = Bun.file("/certs/server.key");
const tlsCert = Bun.file("/certs/server.crt");
if (!(await tlsKey.exists()) || !(await tlsCert.exists())) {
    console.error("no /certs mounted — HTTP/3 needs TLS, refusing to start");
    process.exit(1);
}

Bun.serve({
    hostname: "0.0.0.0",
    port: 8443,
    // Every worker process binds the same port, on UDP as well as TCP, and the
    // kernel spreads connections across them by 4-tuple. That is how this entry
    // uses more than one core, and it holds for QUIC because the load generator
    // does not migrate a connection to a new address mid-run.
    reusePort: true,
    development: false,
    tls: { key: tlsKey, cert: tlsCert },
    // Experimental in Bun, and the reason this entry exists: QUIC on udp/8443,
    // HTTP/1.1 still on tcp/8443, Alt-Svc advertised on the TCP responses.
    http3: true,
    // A saturated server can take longer than the 10s default.
    idleTimeout: 120,
    routes,
    fetch: () => new Response("Not Found", { status: 404, headers: TEXT }),
});

console.log("Application started.");
