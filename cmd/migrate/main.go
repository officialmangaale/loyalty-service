package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/migrations"
	_ "github.com/lib/pq"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: migrate <up|down|status>")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("database ping failed: %v", err)
	}

	switch os.Args[1] {
	case "up":
		err = migrations.Apply(ctx, db)
	case "down":
		err = migrations.RollbackLast(ctx, db)
	case "status":
		var statuses []migrations.Status
		statuses, err = migrations.Current(ctx, db)
		if err == nil {
			for _, status := range statuses {
				fmt.Printf("%06d %-40s %s %s\n", status.Version, status.Name, status.Checksum, status.AppliedAt.UTC().Format(time.RFC3339))
			}
		}
	default:
		log.Fatal("usage: migrate <up|down|status>")
	}
	if err != nil {
		log.Fatal(err)
	}
}
