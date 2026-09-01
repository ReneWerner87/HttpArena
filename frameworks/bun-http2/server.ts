// Bun's own HTTP/2 server: Bun.serve with `http2: true`, no framework on top.
//
// Bun.serve grew HTTP/2 as an experimental flag - the same routes, the same
// fetch handler, the same Response objects, with the protocol picked per
// connection instead of per server. That is the whole point of this entry:
// nothing below is h2-aware. The router, the JSON shape and the static handler
// are the ones the plain `bun` entry runs over HTTP/1.1, so a number here reads
// against that one as the cost of the protocol rather than of a second
// implementation.
//
// Two listeners, because the four subscribed profiles ask for two transports:
//
//   :8443  TLS, ALPN picks h2      - baseline-h2, static-h2
//   :8082  cleartext, h2 preface   - baseline-h2c, json-h2c
//
// Both are one `http2: true` away from the HTTP/1.1 listener the plain entry
// opens. Over TLS Bun offers h2 in ALPN and falls back to HTTP/1.1 for clients
// that do not ask for it; in cleartext it reads the connection preface and
// switches, which is prior-knowledge h2c - no Upgrade dance.

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

// A missing dataset is not fatal: /json answers with an empty list so the
// baseline profiles still run.
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

    // Bun.serve does no content negotiation on either protocol, so the body is
    // compressed here and only when the client asked for it: a Content-Encoding
    // nobody accepted is a validation failure. json-h2c does not ask, so this
    // path is the plain one during that profile.
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
// sniffing: static-h2 checks the header on woff2 and webp among others.
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

const listener = {
    hostname: "0.0.0.0",
    // Every worker process binds the same port and the kernel spreads the
    // accepts, which is how this entry uses more than one core.
    reusePort: true,
    development: false,
    // Experimental in Bun, and the reason this entry exists.
    http2: true,
    // A saturated server can take longer than the 10s default.
    idleTimeout: 120,
    routes,
    fetch: () => new Response("Not Found", { status: 404, headers: TEXT }),
};

// h2c on :8082 — cleartext, prior knowledge. Bun switches a connection to
// HTTP/2 when it opens with the h2 preface and answers HTTP/1.1 otherwise, so
// this is the same listener the plain entry opens with one flag added.
Bun.serve({ ...listener, port: 8082 });

// h2 on :8443 — TLS, ALPN. The harness mounts /certs for every profile that
// needs a certificate; without them there is nothing to serve h2 over, so the
// listener is simply not opened and the h2c half still runs.
const tlsKey = Bun.file("/certs/server.key");
const tlsCert = Bun.file("/certs/server.crt");
if (await tlsKey.exists() && await tlsCert.exists()) {
    Bun.serve({ ...listener, port: 8443, tls: { key: tlsKey, cert: tlsCert } });
} else {
    console.log("no /certs mounted — h2 over TLS on :8443 not started");
}

console.log("Application started.");
