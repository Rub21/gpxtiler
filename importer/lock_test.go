package importer

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestLockWaits(t *testing.T) {
	dsn := os.Getenv("GPXTILER_TEST_DB")
	if dsn == "" {
		t.Skip("GPXTILER_TEST_DB is not set")
	}
	ctx := context.Background()
	a, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	b, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)

	if err := Lock(ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	// Without waiting, the second run gives up.
	if err := Lock(ctx, b, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	// With a wait, it gets the lock when the first run ends.
	go func() {
		time.Sleep(time.Second)
		a.Close(ctx)
	}()
	start := time.Now()
	if err := Lock(ctx, b, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("waited too long")
	}
}
