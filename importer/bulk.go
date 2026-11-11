package importer

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// The first load into an empty database is a bulk load, like osm2pgsql
// does: the indexes are created at the end, not kept up to date row by
// row.
//
//   - bulkStart: drop the index of trace_segments and the primary key and
//     index of each grid table, and create an UNLOGGED table per zoom,
//     grid_raw_z<N>, that keeps every edge of every trace (no ON CONFLICT).
//   - flush: COPY the edges into grid_raw_z<N>.
//   - bulkFinish: count the edges with one GROUP BY per zoom into
//     trace_grid_z<N>, create the primary keys and indexes again and drop
//     the raw tables.
//
// The result is the same as the normal path: adding 1 for each trace with
// ON CONFLICT, or counting all the traces at the end, gives the same n.
//
// A bulk load that stops in the middle can not go on: the database has
// traces but the grid tables have no primary key. checkGrid finds it, and
// the load must start again with an empty database.

// rawTable is the name of the table that keeps the raw edges of a zoom
// during a bulk load.
func rawTable(z int) string {
	return fmt.Sprintf("grid_raw_z%d", z)
}

// gridGeomSQL is the SQL of the line between two grid nodes at zoom z.
func gridGeomSQL(z int) string {
	cell := strconv.FormatFloat(LowTolerance(z), 'g', -1, 64)
	return fmt.Sprintf("ST_SetSRID(ST_MakeLine(ST_MakePoint(x0 * %[1]s, y0 * %[1]s), ST_MakePoint(x1 * %[1]s, y1 * %[1]s)), 3857)", cell)
}

func bulkStart(ctx context.Context, conn *pgx.Conn, zooms []int) error {
	sql := "DROP INDEX IF EXISTS trace_segments_geom_idx;\n"
	for _, z := range zooms {
		t := LowTable(z)
		sql += fmt.Sprintf(`
			DROP TABLE IF EXISTS %[2]s;
			CREATE UNLOGGED TABLE %[2]s (x0 int, y0 int, x1 int, y1 int);
			TRUNCATE %[1]s;
			ALTER TABLE %[1]s DROP CONSTRAINT IF EXISTS %[1]s_pkey;
			DROP INDEX IF EXISTS %[1]s_geom_idx;`, t, rawTable(z))
	}
	return execMulti(ctx, conn.PgConn(), sql)
}

func bulkFinish(ctx context.Context, conn *pgx.Conn, zooms []int, logger *log.Logger) error {
	pg := conn.PgConn()
	// The GROUP BY sorts all the edges of a zoom: give it memory.
	if err := execMulti(ctx, pg, "SET work_mem = '256MB'"); err != nil {
		return err
	}
	for _, z := range zooms {
		t := LowTable(z)
		start := time.Now()
		logger.Printf("grid z%d: counting the edges", z)
		err := execMulti(ctx, pg, fmt.Sprintf(`
			INSERT INTO %[1]s (x0, y0, x1, y1, n, geom)
			SELECT x0, y0, x1, y1, count(*), %[3]s
			  FROM %[2]s GROUP BY 1, 2, 3, 4`, t, rawTable(z), gridGeomSQL(z)))
		if err != nil {
			return fmt.Errorf("grid z%d: %w", z, err)
		}
		logger.Printf("grid z%d: creating the indexes (%s so far)", z, time.Since(start).Round(time.Second))
		err = execMulti(ctx, pg, fmt.Sprintf(`
			ALTER TABLE %[1]s ADD PRIMARY KEY (x0, y0, x1, y1);
			CREATE INDEX IF NOT EXISTS %[1]s_geom_idx ON %[1]s USING gist (geom);
			DROP TABLE %[2]s;
			ANALYZE %[1]s`, t, rawTable(z)))
		if err != nil {
			return fmt.Errorf("grid z%d: %w", z, err)
		}
		logger.Printf("grid z%d: done in %s", z, time.Since(start).Round(time.Second))
	}
	start := time.Now()
	logger.Printf("trace_segments: creating the index")
	err := execMulti(ctx, pg, `
		CREATE INDEX IF NOT EXISTS trace_segments_geom_idx ON trace_segments USING gist (geom);
		ANALYZE trace_segments;
		RESET work_mem`)
	if err != nil {
		return err
	}
	logger.Printf("trace_segments: done in %s", time.Since(start).Round(time.Second))
	return nil
}

// checkGrid returns an error when a grid table has no primary key and the
// database has traces: a bulk load stopped in the middle.
func checkGrid(ctx context.Context, conn *pgx.Conn, zooms []int) error {
	for _, z := range zooms {
		t := LowTable(z)
		var ok bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1)`, t+"_pkey").Scan(&ok)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM traces").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%s has no primary key: the first load did not finish; start again with an empty database", t)
		}
	}
	return nil
}
