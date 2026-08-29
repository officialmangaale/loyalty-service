package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestFindReservationByIdempotencyUsesPlatformIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()

	userID := uuid.New()
	accountID := uuid.New()
	reservationID := uuid.New()
	rewardID := uuid.New()
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT reservation_id, account_id, idempotency_key, checkout_reference,`).
		WithArgs(userID, "retry-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"reservation_id", "account_id", "idempotency_key", "checkout_reference",
			"order_id", "restaurant_id", "status", "points", "discount_minor", "currency",
			"reward_id", "policy_version", "funding_source", "expires_at", "captured_at",
			"released_at", "version", "created_at", "updated_at",
		}).AddRow(
			reservationID, accountID, "retry-1", "checkout-1", nil, int64(42), "ACTIVE",
			int64(100), int64(1000), "INR", rewardID, "reward-v1", FundingMangaale,
			now.Add(time.Minute), nil, nil, int64(0), now, now,
		))

	reservation, found, err := NewStore(db).FindReservationByIdempotency(context.Background(), userID, "retry-1")
	if err != nil {
		t.Fatalf("find reservation: %v", err)
	}
	if !found || reservation.ReservationID != reservationID || reservation.AccountID != accountID ||
		reservation.RewardID == nil || *reservation.RewardID != rewardID {
		t.Fatalf("unexpected reservation: found=%v reservation=%#v", found, reservation)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
