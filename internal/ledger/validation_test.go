package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

func TestCommandValidation(t *testing.T) {
	now := time.Now().UTC()
	validPending := PendingEarnCommand{
		PlatformUserID: uuid.New(),
		OrderID:        "12345",
		EffectKey:      "base",
		Points:         100,
		FundingSource:  FundingMangaale,
		PolicyVersion:  "earn-v1",
		IdempotencyKey: "event-1",
		OccurredAt:     now,
	}
	if err := validatePendingEarn(validPending); err != nil {
		t.Fatalf("valid pending earn rejected: %v", err)
	}

	invalidPending := validPending
	invalidPending.EffectKey = "contains spaces"
	if !errors.Is(validatePendingEarn(invalidPending), ErrInvalidCommand) {
		t.Fatal("invalid effect key was accepted")
	}

	validReserve := ReserveCommand{
		PlatformUserID:    uuid.New(),
		CheckoutReference: "checkout-1",
		Points:            50,
		DiscountMinor:     500,
		Currency:          "INR",
		PolicyVersion:     "redemption-v1",
		FundingSource:     FundingMangaale,
		IdempotencyKey:    "reserve-1",
		OccurredAt:        now,
		ExpiresAt:         now.Add(5 * time.Minute),
	}
	if err := validateReserve(validReserve); err != nil {
		t.Fatalf("valid reserve rejected: %v", err)
	}
	tooLong := validReserve
	tooLong.ExpiresAt = now.Add(16 * time.Minute)
	if !errors.Is(validateReserve(tooLong), ErrInvalidCommand) {
		t.Fatal("overlong reservation was accepted")
	}
	invalidCurrency := validReserve
	invalidCurrency.Currency = "RUPEES"
	if !errors.Is(validateReserve(invalidCurrency), ErrInvalidCommand) {
		t.Fatal("invalid currency was accepted")
	}
}

func TestEntryCursorRoundTrip(t *testing.T) {
	want := &EntryCursor{
		CreatedAt: time.Date(2026, 8, 29, 1, 2, 3, 456, time.UTC),
		LedgerID:  uuid.New(),
	}
	encoded := FormatEntryCursor(want)
	got, err := ParseEntryCursor(encoded)
	if err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.LedgerID != want.LedgerID {
		t.Fatalf("cursor mismatch: got %#v want %#v", got, want)
	}
	if _, err := ParseEntryCursor("not-a-cursor"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("malformed cursor was accepted")
	}
}

func TestIdempotencyAndRetryHelpers(t *testing.T) {
	entry := Entry{
		AccountID:      uuid.New(),
		Type:           EntryOrderPendingEarn,
		PendingDelta:   10,
		IdempotencyKey: "same-key",
		ReferenceType:  "ORDER",
		ReferenceID:    "42",
		PolicyVersion:  "v1",
	}
	if !sameEffect(entry, entry) {
		t.Fatal("identical effects were not considered equal")
	}
	different := entry
	different.PendingDelta++
	if sameEffect(entry, different) {
		t.Fatal("different accounting effects were considered equal")
	}
	if !isRetryableTransactionError(&pq.Error{Code: "40001"}) {
		t.Fatal("serialization failure was not retryable")
	}
	if !isRetryableTransactionError(&pq.Error{Code: "40P01"}) {
		t.Fatal("deadlock was not retryable")
	}
	if isRetryableTransactionError(errors.New("ordinary failure")) {
		t.Fatal("ordinary error was considered retryable")
	}
}

func TestLengthLimitsProtectDatabaseKeys(t *testing.T) {
	command := PendingEarnCommand{
		PlatformUserID: uuid.New(),
		OrderID:        strings.Repeat("x", 121),
		EffectKey:      "base",
		Points:         1,
		FundingSource:  FundingMangaale,
		PolicyVersion:  "v1",
		IdempotencyKey: "idempotency",
		OccurredAt:     time.Now().UTC(),
	}
	if !errors.Is(validatePendingEarn(command), ErrInvalidCommand) {
		t.Fatal("oversized order ID was accepted")
	}
}

func TestReleaseAllocationDoesNotRestoreExpiredReservedPoints(t *testing.T) {
	tests := []struct {
		name          string
		points        int64
		debt          int64
		expired       bool
		wantRestore   int64
		wantDebtApply int64
	}{
		{name: "ordinary release", points: 100, wantRestore: 100},
		{name: "debt consumes release", points: 100, debt: 30, wantRestore: 70, wantDebtApply: 30},
		{name: "debt exceeds allocation", points: 100, debt: 150, wantDebtApply: 100},
		{name: "expired points vanish without paying debt", points: 100, debt: 30, expired: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore, debtApplied := releaseAllocationEffect(test.points, test.debt, test.expired)
			if restore != test.wantRestore || debtApplied != test.wantDebtApply {
				t.Fatalf("restore=%d debtApplied=%d, want restore=%d debtApplied=%d",
					restore, debtApplied, test.wantRestore, test.wantDebtApply)
			}
		})
	}
}
