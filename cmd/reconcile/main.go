package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/reconcile"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/migrations"
	_ "github.com/lib/pq"
)

const recordAcknowledgement = "record-findings-only"

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "scan" && os.Args[1] != "record") {
		log.Fatal("usage: reconcile <scan|record> [-limit 100] [-actor-id ID] [-ack record-findings-only]")
	}
	command := os.Args[1]
	flags := flag.NewFlagSet("reconcile "+command, flag.ExitOnError)
	limit := flags.Int("limit", 100, "maximum findings to return (1-1000)")
	actorID := flags.String("actor-id", "", "required reconciler identity for record mode")
	ack := flags.String("ack", "", "required explicit record-mode acknowledgement")
	if err := flags.Parse(os.Args[2:]); err != nil {
		log.Fatal(err)
	}
	if *limit < 1 || *limit > 1000 {
		log.Fatal("limit must be between 1 and 1000")
	}
	if command == "record" && (strings.TrimSpace(*actorID) == "" || *ack != recordAcknowledgement) {
		log.Fatal("record mode requires -actor-id and -ack record-findings-only")
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
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
	statuses, err := migrations.Current(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	if err := migrations.ValidateCurrent(statuses); err != nil {
		log.Fatalf("migration validation failed: %v", err)
	}
	store, err := reconcile.NewStore(db)
	if err != nil {
		log.Fatal(err)
	}
	var report reconcile.Report
	if command == "record" {
		report, err = store.Record(ctx, *limit, *actorID)
	} else {
		report, err = store.Scan(ctx, *limit)
	}
	if err != nil {
		log.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, string(encoded)); err != nil {
		log.Fatal(err)
	}
}
