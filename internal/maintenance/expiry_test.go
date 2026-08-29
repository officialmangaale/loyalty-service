package maintenance

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/google/uuid"
)

type fakeExpiryStore struct {
	reservationID uuid.UUID
	lotID         uuid.UUID
	reservation   ledger.ReservationTransitionCommand
	lotKey        string
}

func (f *fakeExpiryStore) ListExpiredReservationIDs(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return []uuid.UUID{f.reservationID}, nil
}

func (f *fakeExpiryStore) ExpireReservation(_ context.Context, command ledger.ReservationTransitionCommand) (ledger.ReservationResult, error) {
	f.reservation = command
	return ledger.ReservationResult{}, nil
}

func (f *fakeExpiryStore) ListExpirableLotIDs(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return []uuid.UUID{f.lotID}, nil
}

func (f *fakeExpiryStore) ExpireLot(_ context.Context, _ uuid.UUID, key string, _ time.Time) (ledger.Result, error) {
	f.lotKey = key
	return ledger.Result{}, nil
}

func TestExpiryWorkerUsesStableIdempotencyKeys(t *testing.T) {
	store := &fakeExpiryStore{reservationID: uuid.New(), lotID: uuid.New()}
	worker, err := NewExpiryWorker(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	worker.now = func() time.Time { return time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC) }
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run expiry: %v", err)
	}
	if store.reservation.IdempotencyKey != "reservation-expiry:"+store.reservationID.String() ||
		store.lotKey != "lot-expiration:"+store.lotID.String() {
		t.Fatalf("unstable keys: reservation=%q lot=%q", store.reservation.IdempotencyKey, store.lotKey)
	}
}
