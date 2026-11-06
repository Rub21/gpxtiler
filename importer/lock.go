package importer

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockKey is the PostgreSQL advisory lock that import and update share.
const lockKey = 0x67707874696c6572 // "gpxtiler"

// ErrLocked means another import or update is running on the same database.
var ErrLocked = errors.New("another gpxtiler import or update is running")

// Lock takes the advisory lock for this connection. Only one import or
// update runs at a time, so they never write the same traces or rename
// each other's expire files. The lock ends when the connection closes.
// When another run has the lock, Lock tries again every few seconds for up
// to wait, then returns ErrLocked.
func Lock(ctx context.Context, conn *pgx.Conn, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(lockKey)).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			return ErrLocked
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(5*time.Second, time.Until(deadline))):
		}
	}
}
