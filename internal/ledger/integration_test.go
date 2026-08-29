package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/reconcile"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/migrations"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func TestPostgreSQLAccountingLifecycle(t *testing.T) {
	db := disposableTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	store := ledger.NewStore(db)
	userID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	pending, err := store.RecordPendingEarn(ctx, ledger.PendingEarnCommand{
		PlatformUserID: userID,
		OrderID:        "integration-order-1",
		EffectKey:      "base",
		Points:         100,
		FundingSource:  ledger.FundingMangaale,
		PolicyVersion:  "integration-v1",
		IdempotencyKey: "placed-event-1",
		OccurredAt:     now,
	})
	if err != nil {
		t.Fatalf("record pending earn: %v", err)
	}
	if pending.Account.PendingPoints != 100 || pending.Account.AvailablePoints != 0 {
		t.Fatalf("unexpected pending balance: %#v", pending.Account)
	}

	available, err := store.MakePendingAvailable(ctx, ledger.PendingTransitionCommand{
		PendingBusinessKey: pending.Entry.BusinessKey,
		IdempotencyKey:     "completed-event-1",
		OccurredAt:         now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("make earn available: %v", err)
	}
	if available.Account.AvailablePoints != 100 || available.Account.PendingPoints != 0 {
		t.Fatalf("unexpected available balance: %#v", available.Account)
	}

	reservation, err := store.ReservePoints(ctx, ledger.ReserveCommand{
		PlatformUserID:    userID,
		CheckoutReference: "integration-checkout-1",
		Points:            60,
		DiscountMinor:     600,
		Currency:          "INR",
		PolicyVersion:     "integration-redemption-v1",
		FundingSource:     ledger.FundingMangaale,
		IdempotencyKey:    "reserve-1",
		OccurredAt:        now.Add(2 * time.Minute),
		ExpiresAt:         now.Add(7 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve points: %v", err)
	}
	if reservation.Account.AvailablePoints != 40 || reservation.Account.ReservedPoints != 60 {
		t.Fatalf("unexpected reserved balance: %#v", reservation.Account)
	}

	released, err := store.ReleaseReservation(ctx, ledger.ReservationTransitionCommand{
		ReservationID:  reservation.Reservation.ReservationID,
		IdempotencyKey: "release-1",
		OccurredAt:     now.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("release reservation: %v", err)
	}
	if released.Account.AvailablePoints != 100 || released.Account.ReservedPoints != 0 {
		t.Fatalf("unexpected released balance: %#v", released.Account)
	}

	replay, err := store.RecordPendingEarn(ctx, ledger.PendingEarnCommand{
		PlatformUserID: userID,
		OrderID:        "integration-order-1",
		EffectKey:      "base",
		Points:         100,
		FundingSource:  ledger.FundingMangaale,
		PolicyVersion:  "integration-v1",
		IdempotencyKey: "placed-event-1",
		OccurredAt:     now,
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("pending replay failed: replay=%v err=%v", replay.Replayed, err)
	}

	testConcurrentReservation(t, ctx, store, userID, now)
	reconciler, err := reconcile.NewStore(db)
	if err != nil {
		t.Fatalf("create reconciler: %v", err)
	}
	report, err := reconciler.Scan(ctx, 100)
	if err != nil {
		t.Fatalf("reconcile accounting lifecycle: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("accounting lifecycle produced reconciliation findings: %#v", report.Findings)
	}
}

func testConcurrentReservation(t *testing.T, ctx context.Context, store *ledger.Store, userID uuid.UUID, now time.Time) {
	t.Helper()
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.ReservePoints(ctx, ledger.ReserveCommand{
				PlatformUserID:    userID,
				CheckoutReference: "concurrent-" + string(rune('a'+index)),
				Points:            80,
				DiscountMinor:     800,
				Currency:          "INR",
				PolicyVersion:     "integration-redemption-v1",
				FundingSource:     ledger.FundingMangaale,
				IdempotencyKey:    "concurrent-reserve-" + string(rune('a'+index)),
				OccurredAt:        now.Add(4 * time.Minute),
				ExpiresAt:         now.Add(9 * time.Minute),
			})
			errorsFound <- err
		}(index)
	}
	wait.Wait()
	close(errorsFound)
	var successes, insufficient int
	for err := range errorsFound {
		if err == nil {
			successes++
		} else if errors.Is(err, ledger.ErrInsufficientPoints) {
			insufficient++
		} else {
			t.Fatalf("unexpected concurrent reservation error: %v", err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("concurrent reservation outcome: successes=%d insufficient=%d", successes, insufficient)
	}
}

func disposableTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	databaseURL := os.Getenv("LOYALTY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set LOYALTY_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	databaseName := strings.TrimPrefix(parsed.Path, "/")
	if !strings.Contains(strings.ToLower(databaseName), "test") {
		t.Fatalf("refusing integration test against database without 'test' in its name")
	}
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP SCHEMA IF EXISTS loyalty CASCADE`)
		_ = db.Close()
	})
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS loyalty CASCADE`); err != nil {
		t.Fatalf("reset test schema: %v", err)
	}
	return db
}
