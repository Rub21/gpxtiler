// Command gpxtiler loads the OSM GPX dump into PostGIS and serves tiles.
package main

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rub21/gpxtiler/importer"
	"github.com/Rub21/gpxtiler/schema"
	"github.com/Rub21/gpxtiler/tiles"
)

const usage = `usage:
  gpxtiler import [flags] FILE   load a GPX dump (.gpx, .gpx.bz2, .gpx.gz, an http(s) URL, or - for stdin)
  gpxtiler update -rss [flags]   load new traces from the website RSS feed
  gpxtiler update -ids LIST      delete traces that are not in the id list, load the missing ones
  gpxtiler serve [flags]         serve the PNG tiles on /lines/{z}/{x}/{y}.png
  gpxtiler render                render low zoom tiles (not ready yet)

Run "gpxtiler import -h", "gpxtiler update -h" or "gpxtiler serve -h" to see the flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = runImport(os.Args[2:])
	case "update":
		err = runUpdate(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "render":
		err = fmt.Errorf("%s is not ready yet", os.Args[1])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// commonFlags adds the flags that import and update share.
func commonFlags(fs *flag.FlagSet, cfg *importer.Config) *string {
	db := fs.String("db", envOr("GPXTILER_DB", "postgres://localhost/gpxtiler"), "PostgreSQL connection string (env GPXTILER_DB)")
	fs.IntVar(&cfg.BatchSize, "batch", cfg.BatchSize, "traces per transaction")
	fs.IntVar(&cfg.MaxPoints, "max-points", cfg.MaxPoints, "cut lines after this many points")
	fs.Float64Var(&cfg.MaxLength, "max-length", cfg.MaxLength, "cut lines after this many meters")
	fs.IntVar(&cfg.LowZoom, "low-zoom", cfg.LowZoom, "zoom used to simplify trace_segments_low")
	fs.StringVar(&cfg.ExpireDir, "expire-dir", envOr("GPXTILER_EXPIRE_DIR", ""), "folder for expire files, empty to disable (env GPXTILER_EXPIRE_DIR)")
	fs.IntVar(&cfg.ExpireMinZoom, "expire-min-zoom", envInt("GPXTILER_EXPIRE_MIN_ZOOM", cfg.ExpireMinZoom), "lowest zoom in the expire files (env GPXTILER_EXPIRE_MIN_ZOOM)")
	fs.IntVar(&cfg.ExpireMaxZoom, "expire-max-zoom", envInt("GPXTILER_EXPIRE_MAX_ZOOM", cfg.ExpireMaxZoom), "highest zoom in the expire files (env GPXTILER_EXPIRE_MAX_ZOOM)")
	fs.DurationVar(&lockWait, "lock-wait", lockWait, "when another import or update is running, wait this long for it")
	return db
}

// lockWait is how long import and update wait for another run to finish.
var lockWait = 30 * time.Minute

// connect opens the database, creates the tables and takes the lock.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := schema.Apply(ctx, conn); err != nil {
		conn.Close(context.Background())
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := importer.Lock(ctx, conn, lockWait); err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	return conn, nil
}

func runUpdate(args []string) error {
	cfg := importer.DefaultConfig()
	cfg.BatchSize = 50
	rc := importer.DefaultRSSConfig()
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	db := commonFlags(fs, &cfg)
	rss := fs.Bool("rss", false, "load new traces from the RSS feed of the website")
	ids := fs.String("ids", "", "sync with this list of identifiable trace ids (file or http(s) URL)")
	site := fs.String("site", envOr("GPXTILER_SITE", "https://www.openstreetmap.org"), "website to read the traces from (env GPXTILER_SITE)")
	delay := fs.Duration("delay", 500*time.Millisecond, "pause between requests to the website")
	fs.IntVar(&rc.MaxTraces, "max-traces", rc.MaxTraces, "download or check at most this many traces per run")
	fs.DurationVar(&rc.MaxTime, "max-time", rc.MaxTime, "stop the run after this time")
	fs.Int64Var(&rc.StartID, "start-id", 0, "-rss, first run only: start after this trace id (default: highest id in the database)")
	fs.Float64Var(&cfg.MaxDeleteRatio, "max-delete-ratio", cfg.MaxDeleteRatio, "-ids: stop if more than this share of traces would be deleted, 0 to disable")
	fs.Parse(args)
	if *rss == (*ids != "") || fs.NArg() != 0 {
		return fmt.Errorf("update needs -rss or -ids LIST\n\n%s", usage)
	}

	// Read the list before taking the lock: it can be a big download.
	var list []int64
	if *ids != "" {
		in, closeIn, err := openInput(*ids)
		if err != nil {
			return err
		}
		list, err = importer.ReadIDList(in)
		closeIn()
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn, err := connect(ctx, *db)
	if errors.Is(err, importer.ErrLocked) {
		// A long import is running. Not an error: the next run goes on.
		log.Printf("%v after %s, skipping this run", err, lockWait)
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	f := importer.NewFetcher(*site)
	f.Delay = *delay
	start := time.Now()

	if *ids != "" {
		ic := importer.IDsConfig{MaxTraces: rc.MaxTraces, MaxTime: rc.MaxTime}
		st, err := importer.RunIDs(ctx, conn, f, list, cfg, ic)
		log.Printf("done in %s: %d deleted, %d new, %d skipped, %d left for the next run, %d segments, %d expired tiles %s",
			time.Since(start).Round(time.Second), st.Deleted, st.Inserted, st.Skipped, st.Left,
			st.Segments, st.ExpireTiles, st.ExpireFile)
		return err
	}

	st, err := importer.RunRSS(ctx, conn, f, cfg, rc)
	if st.Checked == 0 {
		return err
	}
	log.Printf("done in %s: ids %d to %d, %d checked, %d new, %d skipped, %d segments, %d expired tiles %s",
		time.Since(start).Round(time.Second), st.FromID, st.ToID, st.Checked, st.Inserted, st.Skipped,
		st.Segments, st.ExpireTiles, st.ExpireFile)
	return err
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", envOr("GPXTILER_DB", "postgres://localhost/gpxtiler"), "PostgreSQL connection string (env GPXTILER_DB)")
	listen := fs.String("listen", envOr("GPXTILER_LISTEN", ":8080"), "address to listen on (env GPXTILER_LISTEN)")
	lowZoom := fs.Int("low-zoom", importer.DefaultConfig().LowZoom, "zooms up to this one read trace_segments_low (same as import -low-zoom)")
	minLive := fs.Int("min-live-zoom", 6, "tiles below this zoom are read from -tile-dir, not drawn on the fly")
	maxZoom := fs.Int("max-zoom", 20, "highest zoom")
	colors := fs.Int("colors", envInt("GPXTILER_COLORS", 64), "colors per PNG, like pngquant on the current tiles; 0 for full color (env GPXTILER_COLORS)")
	tileDir := fs.String("tile-dir", envOr("GPXTILER_TILE_DIR", ""), "folder with the low zoom tiles from render (env GPXTILER_TILE_DIR)")
	maxRenders := fs.Int("max-renders", runtime.NumCPU(), "tiles drawn at the same time")
	maxAge := fs.Duration("max-age", time.Hour, "Cache-Control max-age of the tiles")
	timeout := fs.Duration("timeout", 30*time.Second, "stop drawing a tile after this time")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("serve takes no arguments\n\n%s", usage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A pool, not connect(): serve runs all the time and must not take the
	// import/update lock. It only reads.
	pcfg, err := pgxpool.ParseConfig(*db)
	if err != nil {
		return err
	}
	pcfg.MaxConns = int32(*maxRenders + 2)
	pcfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := &tiles.Server{
		Renderer:    &tiles.Renderer{Pool: pool, LowZoom: *lowZoom, Colors: *colors},
		MinLiveZoom: *minLive,
		MaxZoom:     *maxZoom,
		TileDir:     *tileDir,
		MaxAge:      *maxAge,
		Timeout:     *timeout,
		Log:         log.Default(),
	}
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(*maxRenders), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	log.Printf("serving tiles on %s", *listen)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runImport(args []string) error {
	cfg := importer.DefaultConfig()
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	db := commonFlags(fs, &cfg)
	fs.Float64Var(&cfg.MaxDeleteRatio, "max-delete-ratio", cfg.MaxDeleteRatio, "stop if more than this share of traces would be deleted, 0 to disable")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("import needs one FILE\n\n%s", usage)
	}

	in, closeIn, err := openInput(fs.Arg(0))
	if err != nil {
		return err
	}
	defer closeIn()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn, err := connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	start := time.Now()
	st, err := importer.Run(ctx, conn, in, cfg)
	if err != nil {
		return err
	}
	log.Printf("done in %s: %d new, %d updated, %d deleted, %d segments, %d expired tiles %s",
		time.Since(start).Round(time.Second), st.Inserted, st.Updated, st.Deleted, st.Segments,
		st.ExpireTiles, st.ExpireFile)
	return nil
}

// openInput opens a local file, an http(s) URL or stdin ("-"), and
// unpacks it by the name: .bz2 or .gz.
func openInput(name string) (io.Reader, func(), error) {
	var f io.ReadCloser = os.Stdin
	switch {
	case strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://"):
		resp, err := http.Get(name)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("GET %s: %s", name, resp.Status)
		}
		f = resp.Body
	case name != "-":
		file, err := os.Open(name)
		if err != nil {
			return nil, nil, err
		}
		f = file
	}
	closeFn := func() { f.Close() }
	r := bufio.NewReaderSize(f, 1<<20)
	switch {
	case strings.HasSuffix(name, ".bz2"):
		// Go's bzip2 is slow; for big files pipe lbzip2 -dc into "-".
		return bzip2.NewReader(r), closeFn, nil
	case strings.HasSuffix(name, ".gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return gz, closeFn, nil
	}
	return r, closeFn, nil
}

// envInt reads an integer from the environment, or def when unset or bad.
func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
