// Command gpxtiler loads the OSM GPX dump into PostGIS and serves tiles.
package main

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
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
  gpxtiler import [flags] FILE   load a GPX dump (.gpx, .gpx.bz2, .gpx.gz, or - for stdin)
  gpxtiler serve                 serve tiles (not ready yet)
  gpxtiler render                render low zoom tiles (not ready yet)

Run "gpxtiler import -h" to see the flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = runImport(os.Args[2:])
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

func runImport(args []string) error {
	cfg := importer.DefaultConfig()
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	db := fs.String("db", envOr("GPXTILER_DB", "postgres://localhost/gpxtiler"), "PostgreSQL connection string (env GPXTILER_DB)")
	fs.IntVar(&cfg.BatchSize, "batch", cfg.BatchSize, "traces per transaction")
	fs.IntVar(&cfg.MaxPoints, "max-points", cfg.MaxPoints, "cut lines after this many points")
	fs.Float64Var(&cfg.MaxLength, "max-length", cfg.MaxLength, "cut lines after this many meters")
	fs.IntVar(&cfg.LowZoom, "low-zoom", cfg.LowZoom, "zoom used to simplify trace_segments_low")
	fs.StringVar(&cfg.ExpireDir, "expire-dir", envOr("GPXTILER_EXPIRE_DIR", ""), "folder for expire files, empty to disable (env GPXTILER_EXPIRE_DIR)")
	fs.IntVar(&cfg.ExpireZoom, "expire-zoom", cfg.ExpireZoom, "zoom of the tiles in the expire files")
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
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if err := schema.Apply(ctx, conn); err != nil {
		return fmt.Errorf("schema: %w", err)
	}

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

func openInput(name string) (io.Reader, func(), error) {
	f := os.Stdin
	if name != "-" {
		var err error
		if f, err = os.Open(name); err != nil {
			return nil, nil, err
		}
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
