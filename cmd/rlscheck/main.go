// Command rlscheck is the deploy gate: it exits 1 when any public table in
// the database at DATABASE_URL (or the first argument) has RLS disabled.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/intouch/api/internal/rlsgate"
	"github.com/jackc/pgx/v5"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if len(os.Args) > 1 {
		url = os.Args[1]
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "rlscheck: set DATABASE_URL or pass a database URL")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rlscheck: connect:", err)
		os.Exit(2)
	}
	defer conn.Close(ctx)
	if err := rlsgate.Check(ctx, conn); err != nil {
		fmt.Fprintln(os.Stderr, "rlscheck: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("rlscheck: ok, every public table has row level security enabled")
}
