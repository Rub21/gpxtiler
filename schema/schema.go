// Package schema creates the PostGIS tables used by gpxtiler.
package schema

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var SQL string

// Apply creates the tables and indexes if they do not exist.
func Apply(ctx context.Context, conn *pgx.Conn) error {
	// Simple protocol, so the file can have many statements.
	_, err := conn.PgConn().Exec(ctx, SQL).ReadAll()
	return err
}
