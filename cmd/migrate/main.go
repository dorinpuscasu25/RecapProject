// Command migrate applies embedded PostgreSQL schema migrations.
package main

import (
	"context"
	"fmt"
	"os"
	"time"
	_ "time/tzdata" // IANA zones (Europe/Chisinau) without OS tzdata

	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "status" {
		v, err := db.MigrationStatus(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			os.Exit(1)
		}
		fmt.Println("schema version:", v)
		return
	}
	if err := db.Migrate(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	v, _ := db.MigrationStatus(ctx, url)
	fmt.Println("migrations applied; schema version:", v)
}
