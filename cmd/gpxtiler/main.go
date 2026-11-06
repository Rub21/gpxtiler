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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rub21/gpxtiler/importer"
	"github.com/Rub21/gpxtiler/schema"
)

const usage = `usage:
  gpxtiler import [flags] FILE   load a GPX dump (.gpx, .gpx.bz2, .gpx.gz, an http(s) URL, or - for stdin)
  gpxtiler update -rss [flags]   load new traces from the website RSS feed
  gpxtiler serve                 serve tiles (not ready yet)
  gpxtiler render                render low zoom tiles (not ready yet)

Run "gpxtiler import -h" or "gpxtiler update -h" to see the flags.`

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
	case "serve", "render":
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
	fs.IntVar(&cfg.ExpireZoom, "expire-zoom", cfg.ExpireZoom, "zoom of the tiles in the expire files")
	return db
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
	if err := importer.Lock(ctx, conn); err != nil {
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
	site := fs.String("site", envOr("GPXTILER_SITE", "https://www.openstreetmap.org"), "website to read the traces from (env GPXTILER_SITE)")
	delay := fs.Duration("delay", 500*time.Millisecond, "pause between requests to the website")
	fs.IntVar(&rc.MaxTraces, "max-traces", rc.MaxTraces, "check at most this many trace ids per run")
	fs.DurationVar(&rc.MaxTime, "max-time", rc.MaxTime, "stop the run after this time")
	fs.Int64Var(&rc.StartID, "start-id", 0, "first run only: start after this trace id (default: highest id in the database)")
	fs.Parse(args)
	if !*rss || fs.NArg() != 0 {
		return fmt.Errorf("update needs -rss\n\n%s", usage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn, err := connect(ctx, *db)
	if errors.Is(err, importer.ErrLocked) {
		// A dump import is running. Not an error: the next timer run goes on.
		log.Print(err)
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	f := importer.NewFetcher(*site)
	f.Delay = *delay
	start := time.Now()
	st, err := importer.RunRSS(ctx, conn, f, cfg, rc)
	if st.Checked == 0 {
		return err
	}
	log.Printf("done in %s: ids %d to %d, %d checked, %d new, %d skipped, %d segments, %d expired tiles %s",
		time.Since(start).Round(time.Second), st.FromID, st.ToID, st.Checked, st.Inserted, st.Skipped,
		st.Segments, st.ExpireTiles, st.ExpireFile)
	return err
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
