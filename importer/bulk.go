package importer

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// The first load into an empty database is a bulk load, like osm2pgsql
// does: the spatial (gist) indexes are created at the end, once, instead
// of being kept up to date row by row. A gist index is the slow part of
// the load: every new line goes to a random place of a big index.
//
// The rows are the same as in a normal load, only the indexes wait. If the
// load stops in the middle, the rows already written are good: the next
// run creates the indexes again (schema.Apply and EnsureLowTables use
// CREATE INDEX IF NOT EXISTS) and goes on as a normal load.

// geomIndexes returns the gist indexes of the tables with lines, as
// "index name" -> "table name".
func geomIndexes(zooms []int) map[string]string {
	idx := map[string]string{"trace_segments_geom_idx": "trace_segments"}
	for _, z := range zooms {
		idx[LowTable(z)+"_geom_idx"] = LowTable(z)
	}
	return idx
}

func bulkStart(ctx context.Context, conn *pgx.Conn, zooms []int) error {
	sql := ""
	for name := range geomIndexes(zooms) {
		sql += fmt.Sprintf("DROP INDEX IF EXISTS %s;\n", name)
	}
	return execMulti(ctx, conn.PgConn(), sql)
}

func bulkFinish(ctx context.Context, conn *pgx.Conn, zooms []int, logger *log.Logger) error {
	// Simplified copies first (small), trace_segments last (big).
	order := make([]string, 0, len(zooms)+1)
	for _, z := range zooms {
		order = append(order, LowTable(z))
	}
	order = append(order, "trace_segments")
	for _, table := range order {
		start := time.Now()
		logger.Printf("%s: creating the index", table)
		err := execMulti(ctx, conn.PgConn(), fmt.Sprintf(`
			CREATE INDEX IF NOT EXISTS %[1]s_geom_idx ON %[1]s USING gist (geom);
			ANALYZE %[1]s`, table))
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		logger.Printf("%s: done in %s", table, time.Since(start).Round(time.Second))
	}
	return nil
}
