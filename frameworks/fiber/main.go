package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const maxBody = 25 * 1024 * 1024

// Prefork is decided at image build time: the Dockerfile takes FIBER_PREFORK as
// a build argument and the fiber-prefork entry builds this same source with it
// set to 1. One child process per core is a worker count past the framework
// default, which standard mode does not allow, so the switch stays off here and
// the tuned sibling is the entry that turns it on.
var preforkEnabled = os.Getenv("FIBER_PREFORK") == "1"

// preforkWorkers is how many processes end up sharing anything the container
// holds once - the connection budget below, most of all. fasthttp forks
// GOMAXPROCS children, read in the master before it spawns anything; a child
// re-runs main() from the top and reads the same value here, because the
// GOMAXPROCS(1) that prefork applies to a child happens later, when it takes
// its listener.
func preforkWorkers() int {
	if !preforkEnabled {
		return 1
	}
	return runtime.GOMAXPROCS(0)
}

type Rating struct {
	Score int `json:"score"`
	Count int `json:"count"`
}

type DatasetItem struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Price    int      `json:"price"`
	Quantity int      `json:"quantity"`
	Active   bool     `json:"active"`
	Tags     []string `json:"tags"`
	Rating   Rating   `json:"rating"`
}

type ProcessedItem struct {
	DatasetItem
	Total int `json:"total"`
}

type ProcessResponse struct {
	Items []ProcessedItem `json:"items"`
	Count int             `json:"count"`
}

var dataset []DatasetItem

func loadDataset() {
	path := os.Getenv("DATASET_PATH")
	if path == "" {
		path = "/data/dataset.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	json.Unmarshal(data, &dataset)
}

func pipeline(c fiber.Ctx) error {
	return c.SendString("ok")
}

// The profile sends a and b and nothing else, so they are read by name through
// Fiber's typed query binder rather than materialising the whole query string
// into a map. Both are hot: baseline drives this endpoint at 4096 connections
// and the two fixed-rate profiles score what a request costs in CPU, where one
// map allocation per request is a line item.
func baseline11(c fiber.Ctx) error {
	sum := fiber.Query[int](c, "a") + fiber.Query[int](c, "b")
	if c.Method() == fiber.MethodPost {
		if n, err := strconv.Atoi(strings.TrimSpace(string(c.Body()))); err == nil {
			sum += n
		}
	}
	return c.SendString(strconv.Itoa(sum))
}

// GET /delay/{ms}: answer no earlier than the wait named in the path. A
// goroutine parked on a timer is what Fiber gives you for free here - the
// handler blocks, the process does not.
func delay(c fiber.Ctx) error {
	ms := fiber.Params[int](c, "ms", -1)
	if ms < 0 {
		return c.SendStatus(fiber.StatusNotFound)
	}
	if ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	return c.SendString(strconv.Itoa(ms))
}

func jsonItems(c fiber.Ctx) error {
	count := fiber.Params[int](c, "count", 0)
	if count < 0 {
		count = 0
	}
	if count > len(dataset) {
		count = len(dataset)
	}
	m := fiber.Query[int](c, "m", 1)
	if m == 0 {
		m = 1
	}

	items := make([]ProcessedItem, count)
	for i := 0; i < count; i++ {
		d := dataset[i]
		items[i] = ProcessedItem{DatasetItem: d, Total: d.Price * d.Quantity * m}
	}
	return c.JSON(ProcessResponse{Items: items, Count: count})
}

func echoBody(c fiber.Ctx) error {
	// fasthttp has already read the body, chunked or not, so the echo is the
	// buffer it holds -- Send sets Content-Length from it.
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	return c.Send(c.Body())
}

var pgPool *pgxpool.Pool
var rdb *redis.Client

const itemColumns = "id, name, category, price, quantity, active, tags, rating_score, rating_count"

// The crud profile reads and writes the same ids, so a long TTL would answer
// from a copy the writes have already moved past.
const crudTTL = 200 * time.Millisecond

// Postgres runs with max_connections=256 and reserves a few of those for the
// superuser, so the entry's share is the budget less that headroom. Under
// prefork the share is divided again: every child process opens a pool of its
// own against the same server, and a pool sized for one process would have the
// fleet asking for sixty-four times what the server will hand out.
func loadPgPool() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return
	}
	budget := 256
	if v := os.Getenv("DATABASE_MAX_CONN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			budget = n
		}
	}
	workers := preforkWorkers()
	maxConns := (budget - 8) / workers
	if m := runtime.NumCPU() * 4 / workers; m < maxConns {
		maxConns = m
	}
	if maxConns < 1 {
		maxConns = 1
	}
	cfg.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return
	}
	pgPool = pool
}

func loadRedis() {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		return
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return
	}
	rdb = redis.NewClient(opt)
}

// tags is a JSONB column, so it comes back as bytes rather than a Go slice.
func queryItems(ctx context.Context, sql string, args ...any) ([]DatasetItem, error) {
	rows, err := pgPool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []DatasetItem{}
	for rows.Next() {
		var it DatasetItem
		var tags []byte
		if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Price, &it.Quantity,
			&it.Active, &tags, &it.Rating.Score, &it.Rating.Count); err != nil {
			continue
		}
		json.Unmarshal(tags, &it.Tags)
		if it.Tags == nil {
			it.Tags = []string{}
		}
		items = append(items, it)
	}
	// A connection that fails mid-iteration ends the loop like a clean finish
	// does. Without this the handler would answer 200 with however many rows
	// arrived before the error, which reads as a short result rather than a
	// failed one.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func queryInt(c fiber.Ctx, name string, fallback int) int {
	if v := c.Query(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// The async-db response is a struct rather than a fiber.Map: same JSON, without
// asking encoding/json to reflect over a map and sort its keys per request.
type itemsResponse struct {
	Items []DatasetItem `json:"items"`
	Count int           `json:"count"`
}

var emptyItems = itemsResponse{Items: []DatasetItem{}}

// Every database and cache call is answered inside this deadline.
//
// Fiber's Ctx.Context() is a background context unless the application puts one
// there, and fasthttp has no per-request cancellation to put there either - a
// client that walks away mid-query leaves the query running and its pool
// connection held. A deadline is what bounds that, and it is what the net/http
// entries get from the request context for free.
const dbTimeout = 5 * time.Second

func dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

func asyncDb(c fiber.Ctx) error {
	if pgPool == nil {
		return c.JSON(emptyItems)
	}
	ctx, cancel := dbContext()
	defer cancel()
	items, err := queryItems(ctx,
		"SELECT "+itemColumns+" FROM items WHERE price BETWEEN $1 AND $2 LIMIT $3",
		queryInt(c, "min", 10), queryInt(c, "max", 50), clamp(queryInt(c, "limit", 50), 1, 50))
	if err != nil {
		return c.JSON(emptyItems)
	}
	return c.JSON(itemsResponse{Items: items, Count: len(items)})
}

func crudList(c fiber.Ctx) error {
	if pgPool == nil {
		return c.Status(500).JSON(fiber.Map{"error": "DB not available"})
	}
	category := c.Query("category")
	if category == "" {
		category = "electronics"
	}
	page := queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}
	limit := clamp(queryInt(c, "limit", 10), 1, 50)
	ctx, cancel := dbContext()
	defer cancel()
	items, err := queryItems(ctx,
		"SELECT "+itemColumns+" FROM items WHERE category = $1 ORDER BY id LIMIT $2 OFFSET $3",
		category, limit, (page-1)*limit)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "query failed"})
	}
	return c.JSON(fiber.Map{"items": items, "total": len(items), "page": page, "limit": limit})
}

type crudBody struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Price    int    `json:"price"`
	Quantity int    `json:"quantity"`
}

func crudCreate(c fiber.Ctx) error {
	if pgPool == nil {
		return c.Status(500).JSON(fiber.Map{"error": "DB not available"})
	}
	var b crudBody
	if err := json.Unmarshal(c.Body(), &b); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "insert failed"})
	}
	if b.Name == "" {
		b.Name = "New Product"
	}
	if b.Category == "" {
		b.Category = "test"
	}
	ctx, cancel := dbContext()
	defer cancel()
	var id int
	err := pgPool.QueryRow(ctx,
		`INSERT INTO items (id, name, category, price, quantity, active, tags, rating_score, rating_count)
		 VALUES ($1, $2, $3, $4, $5, true, '["bench"]', 0, 0)
		 ON CONFLICT (id) DO UPDATE SET name = $2, price = $4, quantity = $5 RETURNING id`,
		b.ID, b.Name, b.Category, b.Price, b.Quantity).Scan(&id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "insert failed"})
	}
	return c.Status(201).JSON(fiber.Map{"id": id, "name": b.Name,
		"category": b.Category, "price": b.Price, "quantity": b.Quantity})
}

// Cache-aside on Redis where the harness provides it - crud is the one profile
// that does.
func crudRead(c fiber.Ctx) error {
	if pgPool == nil {
		return c.Status(500).JSON(fiber.Map{"error": "DB not available"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.SendStatus(404)
	}
	ctx, cancel := dbContext()
	defer cancel()
	key := "crud:" + strconv.Itoa(id)
	if rdb != nil {
		if hit, err := rdb.Get(ctx, key).Result(); err == nil && hit != "" {
			c.Set("X-Cache", "HIT")
			c.Set("Content-Type", "application/json")
			return c.SendString(hit)
		}
	}
	items, err := queryItems(ctx, "SELECT "+itemColumns+" FROM items WHERE id = $1 LIMIT 1", id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "query failed"})
	}
	if len(items) == 0 {
		return c.SendStatus(404)
	}
	body, _ := json.Marshal(items[0])
	if rdb != nil {
		rdb.Set(ctx, key, body, crudTTL)
	}
	c.Set("X-Cache", "MISS")
	c.Set("Content-Type", "application/json")
	return c.Send(body)
}

func crudUpdate(c fiber.Ctx) error {
	if pgPool == nil {
		return c.Status(500).JSON(fiber.Map{"error": "DB not available"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.SendStatus(404)
	}
	var b crudBody
	if err := json.Unmarshal(c.Body(), &b); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "update failed"})
	}
	if b.Name == "" {
		b.Name = "Updated"
	}
	ctx, cancel := dbContext()
	defer cancel()
	tag, err := pgPool.Exec(ctx,
		"UPDATE items SET name = $1, price = $2, quantity = $3 WHERE id = $4",
		b.Name, b.Price, b.Quantity, id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "update failed"})
	}
	if tag.RowsAffected() == 0 {
		return c.SendStatus(404)
	}
	if rdb != nil {
		rdb.Del(ctx, "crud:"+strconv.Itoa(id))
	}
	return c.JSON(fiber.Map{"id": id, "name": b.Name, "price": b.Price, "quantity": b.Quantity})
}

var mimeTypes = map[string]string{
	".css": "text/css", ".js": "application/javascript", ".html": "text/html",
	".woff2": "font/woff2", ".svg": "image/svg+xml", ".webp": "image/webp",
	".json": "application/json",
}

// Static bodies are read from disk on every request, which the static profiles
// require in every mode - nothing here holds a copy, so a file replaced on disk
// is served from its next request onwards.
//
// Fiber's own static middleware is not an option for either half of that.
// Its cache holds open file handles and never re-stats them, so a file replaced
// on disk keeps being served for up to CacheDuration - which is the one thing
// the profile checks. And its Compress option generates .fiber.br twins next to
// the originals rather than reading the .br/.gz ones already there, on a
// directory the harness mounts read-only.
//
// So the twins are picked up here. The profile allows selecting them off
// Accept-Encoding where a framework has no API of its own for it; those bytes
// exist on disk either way, which makes this a file read rather than
// compression. It is also the difference between answering the 20-file rotation
// with 842 KB and with 219 KB, because the compress middleware sits this round
// out: fasthttp's Accept-Encoding matcher compares whole tokens, and the profile
// sends "br;q=1, gzip;q=0.8".
func staticFile(c fiber.Ctx) error {
	name := c.Params("filename")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return c.SendStatus(fiber.StatusNotFound)
	}
	path := "/data/static/" + name

	var data []byte
	enc := ""
	// Fiber's own negotiation reads the q-values the way RFC 9110 says, which
	// is the whole difficulty here. It is guarded on the header being present
	// because with no Accept-Encoding at all a negotiator answers with the
	// first offer, and that would encode a body for a client that never asked.
	if c.Get(fiber.HeaderAcceptEncoding) != "" {
		switch c.AcceptsEncodings("br", "gzip") {
		case "br":
			if b, err := os.ReadFile(path + ".br"); err == nil {
				data, enc = b, "br"
			}
		case "gzip":
			if b, err := os.ReadFile(path + ".gz"); err == nil {
				data, enc = b, "gzip"
			}
		}
	}
	if enc == "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
		data = b
	}

	// The Content-Type is the original file's either way; only the encoding
	// changes. Set before Send so the compress middleware sees a body that is
	// already encoded and leaves it alone.
	ct := mimeTypes[filepath.Ext(name)]
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Set(fiber.HeaderContentType, ct)
	if enc != "" {
		c.Set(fiber.HeaderContentEncoding, enc)
		c.Set(fiber.HeaderVary, fiber.HeaderAcceptEncoding)
	}
	return c.Send(data)
}

func main() {
	// Under prefork the master process supervises children and nothing else: it
	// binds no socket and serves no request, so the dataset, the pool and the
	// cache client belong in the children. Each of those re-runs main() from
	// the top with the marker environment variable set, which is what
	// fiber.IsChild reads.
	serving := !preforkEnabled || fiber.IsChild()
	if serving {
		loadDataset()
		loadPgPool()
		loadRedis()
	}

	app := fiber.New(fiber.Config{
		BodyLimit: maxBody,
	})

	// Compression is mounted on the two routes with a body worth compressing
	// rather than on the whole app. Mounted globally it also runs on /pipeline
	// and /baseline11, whose bodies are a handful of bytes: fasthttp will not
	// compress anything under 200 bytes anyway, so all the middleware does
	// there is walk the chain and stamp a Vary header onto the response - on
	// the endpoint the baseline and the two CPU-per-request profiles drive.
	app.Use([]string{"/json", "/static"}, compress.New())

	app.Get("/pipeline", pipeline)
	app.Get("/baseline11", baseline11)
	app.Post("/baseline11", baseline11)
	app.Get("/json/:count", jsonItems)
	app.Post("/echo", echoBody)
	app.Get("/baseline2", baseline11)
	app.Get("/delay/:ms", delay)
	app.Get("/static/:filename", staticFile)
	app.Get("/async-db", asyncDb)
	app.Get("/crud/items", crudList)
	app.Post("/crud/items", crudCreate)
	app.Get("/crud/items/:id", crudRead)
	app.Put("/crud/items/:id", crudUpdate)

	listen := fiber.ListenConfig{
		DisableStartupMessage: true,
		EnablePrefork:         preforkEnabled,
	}

	if serving {
		// SIGTERM is what `docker stop` sends between profiles, and draining
		// beats being cut off mid-response. It is deliberately not installed in
		// the prefork master: that process serves nothing, and taking the
		// signal over from the runtime there would keep it alive until Docker
		// gave up waiting and escalated to SIGKILL.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		listen.GracefulContext = ctx

		// json-tls, static-tls and 8gbit on 8081, the same app behind TLS. The
		// harness mounts /certs for every run, so the files being there is what
		// says the listener is wanted.
		const cert, key = "/certs/server.crt", "/certs/server.key"
		_, certErr := os.Stat(cert)
		_, keyErr := os.Stat(key)
		if certErr == nil && keyErr == nil {
			go func() {
				// In a child, EnablePrefork means "take the SO_REUSEPORT socket
				// for this address", not "fork again": fasthttp checks the
				// child marker before it looks at anything else. Without it all
				// N children would race for an ordinary bind on 8081 and every
				// one but the winner would fail - silently, before this
				// returned the error rather than dropping it.
				err := app.Listen(":8081", fiber.ListenConfig{
					DisableStartupMessage: true,
					CertFile:              cert,
					CertKeyFile:           key,
					EnablePrefork:         preforkEnabled,
				})
				if err != nil {
					log.Printf("tls listener on :8081: %v", err)
				}
			}()
		}
	}

	if err := app.Listen(":8080", listen); err != nil {
		log.Printf("listener on :8080: %v", err)
		os.Exit(1)
	}
}
