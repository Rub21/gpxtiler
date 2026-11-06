package importer

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// lockKey is the PostgreSQL advisory lock that import and update share.
const lockKey = 0x67707874696c6572 // "gpxtiler"

// ErrLocked means another import or update is running on the same database.
var ErrLocked = errors.New("another gpxtiler import or update is running")

// Lock takes the advisory lock for this connection. Only one import or
// update runs at a time, so they never write the same traces or rename
// each other's expire files. The lock ends when the connection closes.
func Lock(ctx context.Context, conn *pgx.Conn) error {
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(lockKey)).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrLocked
	}
	return nil
}
