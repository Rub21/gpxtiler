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
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
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
  gpxtiler update -ids URL       delete traces that are not in the id list, load the missing ones
  gpxtiler serve [flags]         serve the PNG tiles on /lines/{z}/{x}/{y}.png
  gpxtiler render -zooms 0-9     draw the tiles of these zooms to disk
  gpxtiler render -expired -zooms 6-9   redraw the expired tiles of these zooms, remove the expired ones above

Run "gpxtiler <command> -h" to see the flags.`

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
		err = runRender(os.Args[2:])
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
	fs.Float64Var(&cfg.LowMaxJump, "low-max-jump", cfg.LowMaxJump, "cut the lines at GPS jumps longer than this many meters before building the grid tables, 0 to keep them (must stay the same between import and update)")
	fs.StringVar(&cfg.ExpireDir, "expire-dir", envOr("GPXTILER_EXPIRE_DIR", ""), "folder for the expire files, or several separated by commas (one per reader), empty to disable (env GPXTILER_EXPIRE_DIR)")
	fs.IntVar(&cfg.ExpireMinZoom, "expire-min-zoom", envInt("GPXTILER_EXPIRE_MIN_ZOOM", cfg.ExpireMinZoom), "lowest zoom in the expire files (env GPXTILER_EXPIRE_MIN_ZOOM)")
	fs.IntVar(&cfg.ExpireMaxZoom, "expire-max-zoom", envInt("GPXTILER_EXPIRE_MAX_ZOOM", cfg.ExpireMaxZoom), "highest zoom in the expire files (env GPXTILER_EXPIRE_MAX_ZOOM)")
	fs.DurationVar(&lockWait, "lock-wait", lockWait, "when another import or update is running, wait this long for it")
	return db
}

// lockWait is how long import and update wait for another run to finish.
var lockWait = 30 * time.Minute

// parseZooms reads "5,8,11" into a sorted list of zooms.
func parseZooms(s string) ([]int, error) {
	var zooms []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		z, err := strconv.Atoi(f)
		if err != nil || z < 0 || z > 20 {
			return nil, fmt.Errorf("bad zoom %q", f)
		}
		zooms = append(zooms, z)
	}
	slices.Sort(zooms)
	return slices.Compact(zooms), nil
}

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
	fs.Func("start-id", "-rss, first run only: start after this trace id, or \"latest\" to start at the newest trace in the RSS feed (default: the highest id in the database)", func(v string) error {
		if v == "" {
			return nil
		}
		if v == "latest" {
			rc.StartID = -1
			return nil
		}
		var err error
		rc.StartID, err = strconv.ParseInt(v, 10, 64)
		return err
	})
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
	cacheZoom := fs.Int("cache-zoom", envInt("GPXTILER_CACHE_ZOOM", 14), "tiles up to this zoom are kept in -tile-dir once drawn (env GPXTILER_CACHE_ZOOM)")
	maxZoom := fs.Int("max-zoom", 20, "highest zoom")
	colors := fs.Int("colors", envInt("GPXTILER_COLORS", 64), "colors per PNG, like pngquant on the current tiles; 0 for full color (env GPXTILER_COLORS)")
	tileDir := fs.String("tile-dir", envOr("GPXTILER_TILE_DIR", ""), "folder with the tiles on disk (made by render and by serve), empty to draw everything (env GPXTILER_TILE_DIR)")
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

	rend := &tiles.Renderer{Pool: pool, Colors: *colors}
	if err := rend.LoadLowZooms(ctx); err != nil {
		return err
	}
	log.Printf("grid tables for zooms %v", rend.LowZooms)
	srv := &tiles.Server{
		Renderer:  rend,
		CacheZoom: *cacheZoom,
		MaxZoom:   *maxZoom,
		TileDir:   *tileDir,
		MaxAge:    *maxAge,
		Timeout:   *timeout,
		Log:       log.Default(),
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

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	db := fs.String("db", envOr("GPXTILER_DB", "postgres://localhost/gpxtiler"), "PostgreSQL connection string (env GPXTILER_DB)")
	tileDir := fs.String("tile-dir", envOr("GPXTILER_TILE_DIR", ""), "folder with the tiles on disk (env GPXTILER_TILE_DIR)")
	zooms := fs.String("zooms", "", "zooms to draw, like 0-9")
	expired := fs.Bool("expired", false, "only the tiles in the expire files: redraw the ones in -zooms, remove the ones above (up to -cache-zoom) from disk")
	expireDir := fs.String("expire-dir", envOr("GPXTILER_EXPIRE_DIR", ""), "folder with the expire files from import and update (env GPXTILER_EXPIRE_DIR)")
	cacheZoom := fs.Int("cache-zoom", envInt("GPXTILER_CACHE_ZOOM", 14), "with -expired: highest zoom kept on disk by serve (env GPXTILER_CACHE_ZOOM)")
	workers := fs.Int("workers", runtime.NumCPU(), "tiles drawn at the same time")
	colors := fs.Int("colors", envInt("GPXTILER_COLORS", 64), "colors per PNG (env GPXTILER_COLORS)")
	fs.Parse(args)
	if fs.NArg() != 0 || *tileDir == "" || *zooms == "" || (*expired && *expireDir == "") {
		return fmt.Errorf("render needs -tile-dir and -zooms, and -expire-dir with -expired\n\n%s", usage)
	}
	minZoom, maxZoom, err := parseRange(*zooms)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pcfg, err := pgxpool.ParseConfig(*db)
	if err != nil {
		return err
	}
	pcfg.MaxConns = int32(*workers + 1)
	pcfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	rend := &tiles.Renderer{Pool: pool, Colors: *colors}
	if err := rend.LoadLowZooms(ctx); err != nil {
		return err
	}

	// Only one render writes the tiles at a time; a second one waits.
	lock, err := tiles.Lock(*tileDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	g := &tiles.Generator{Renderer: rend, TileDir: *tileDir, Workers: *workers, Log: log.Default()}
	start := time.Now()
	var st tiles.Stats
	if *expired {
		st, err = g.Expire(ctx, *expireDir, minZoom, maxZoom, *cacheZoom)
	} else {
		st, err = g.All(ctx, minZoom, maxZoom)
	}
	log.Printf("done in %s: %d expire files, %d tiles drawn, %d written, %d deleted",
		time.Since(start).Round(time.Second), st.Files, st.Drawn, st.Written, st.Deleted)
	return err
}

// parseRange reads "a-b" (or "a" for one zoom).
func parseRange(s string) (min, max int, err error) {
	lo, hi, _ := strings.Cut(s, "-")
	if hi == "" {
		hi = lo
	}
	if min, err = strconv.Atoi(lo); err == nil {
		max, err = strconv.Atoi(hi)
	}
	if err != nil || min < 0 || max < min || max > 20 {
		return 0, 0, fmt.Errorf("-zooms must be like 0-9, got %q", s)
	}
	return min, max, nil
}

func runImport(args []string) error {
	cfg := importer.DefaultConfig()
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	db := commonFlags(fs, &cfg)
	fs.Func("grid-zooms", "zooms with a grid table of the lines (trace_grid_z<N>) for the low zoom tiles; update reads them from the database (env GPXTILER_GRID_ZOOMS, default 5,8,11,14)", func(s string) error {
		var err error
		cfg.LowZooms, err = parseZooms(s)
		return err
	})
	if v := os.Getenv("GPXTILER_GRID_ZOOMS"); v != "" {
		var err error
		if cfg.LowZooms, err = parseZooms(v); err != nil {
			log.Fatalf("GPXTILER_GRID_ZOOMS: %v", err)
		}
	}
	fs.Float64Var(&cfg.MaxDeleteRatio, "max-delete-ratio", cfg.MaxDeleteRatio, "stop if more than this share of traces would be deleted, 0 to disable")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "goroutines parsing the dump (default: the number of cores, up to 8)")
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
		// Go's bzip2 is slow and uses one core. lbzip2 (in the Docker image)
		// unpacks with all the cores in another process; Go only parses.
		if lbzip2, err := exec.LookPath("lbzip2"); err == nil {
			cmd := exec.Command(lbzip2, "-dc")
			cmd.Stdin = r
			cmd.Stderr = os.Stderr
			out, err := cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
			}
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			log.Printf("unpacking with lbzip2")
			return out, func() { cmd.Process.Kill(); cmd.Wait(); f.Close() }, nil
		}
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
