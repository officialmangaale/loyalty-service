package ledger

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidCommand      = errors.New("invalid loyalty command")
	ErrNotFound            = errors.New("loyalty record not found")
	ErrInvalidTransition   = errors.New("invalid loyalty state transition")
	ErrIdempotencyConflict = errors.New("idempotency key was reused for a different effect")
	ErrInvariantViolation  = errors.New("loyalty accounting invariant violation")
	ErrInsufficientPoints  = errors.New("insufficient available points")
	ErrAccountRestricted   = errors.New("loyalty account is restricted")
	ErrRewardUnavailable   = errors.New("loyalty reward is unavailable")
	ErrUsageLimitReached   = errors.New("loyalty reward usage limit reached")
)

type EntryType string

const (
	EntryOrderPendingEarn   EntryType = "ORDER_PENDING_EARN"
	EntryOrderAvailable     EntryType = "ORDER_EARN_AVAILABLE"
	EntryOrderPendingCancel EntryType = "ORDER_PENDING_CANCEL"
	EntryRedemptionReserve  EntryType = "REDEMPTION_RESERVE"
	EntryRedemptionRelease  EntryType = "REDEMPTION_RELEASE"
	EntryRedemptionCapture  EntryType = "REDEMPTION_CAPTURE"
	EntryRefundReversal     EntryType = "REFUND_REVERSAL"
	EntryExpiration         EntryType = "EXPIRATION"
)

type FundingSource string

const (
	FundingMangaale   FundingSource = "MANGAALE"
	FundingRestaurant FundingSource = "RESTAURANT"
	FundingShared     FundingSource = "SHARED"
	FundingNone       FundingSource = "NONE"
)

type ActorType string

const (
	ActorSystem     ActorType = "SYSTEM"
	ActorAdmin      ActorType = "ADMIN"
	ActorReconciler ActorType = "RECONCILER"
)

type Account struct {
	AccountID            uuid.UUID
	PlatformUserID       uuid.UUID
	Status               string
	Timezone             string
	AvailablePoints      int64
	PendingPoints        int64
	ReservedPoints       int64
	DebtPoints           int64
	LifetimeEarnedPoints int64
	Version              int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Entry struct {
	LedgerID           uuid.UUID
	AccountID          uuid.UUID
	PlatformUserID     uuid.UUID
	Type               EntryType
	AvailableDelta     int64
	PendingDelta       int64
	ReservedDelta      int64
	DebtDelta          int64
	BusinessKey        string
	IdempotencyKey     string
	SourceLedgerID     *uuid.UUID
	ReferenceType      string
	ReferenceID        string
	OrderID            *string
	RestaurantID       *int64
	CampaignID         *uuid.UUID
	RewardID           *uuid.UUID
	FundingSource      FundingSource
	PolicyVersion      string
	MonetaryValueMinor *int64
	Currency           *string
	ExpiresAt          *time.Time
	OccurredAt         time.Time
	ActorType          ActorType
	ActorID            *string
	Metadata           json.RawMessage
	CreatedAt          time.Time
}

type Result struct {
	Account  Account
	Entry    Entry
	Replayed bool
}

type PendingEarnCommand struct {
	PlatformUserID uuid.UUID
	OrderID        string
	EffectKey      string
	Points         int64
	RestaurantID   *int64
	CampaignID     *uuid.UUID
	FundingSource  FundingSource
	PolicyVersion  string
	IdempotencyKey string
	ExpiresAt      *time.Time
	OccurredAt     time.Time
	Metadata       json.RawMessage
}

type PendingTransitionCommand struct {
	PendingBusinessKey string
	IdempotencyKey     string
	OccurredAt         time.Time
	ActorType          ActorType
	ActorID            *string
	Metadata           json.RawMessage
}

type ReversalCommand struct {
	GrantBusinessKey string
	RefundReference  string
	Points           int64
	IdempotencyKey   string
	PolicyVersion    string
	OccurredAt       time.Time
	ActorType        ActorType
	ActorID          *string
	Metadata         json.RawMessage
}

type Reservation struct {
	ReservationID     uuid.UUID
	AccountID         uuid.UUID
	IdempotencyKey    string
	CheckoutReference string
	OrderID           *string
	RestaurantID      *int64
	Status            string
	Points            int64
	DiscountMinor     int64
	Currency          string
	RewardID          *uuid.UUID
	PolicyVersion     string
	FundingSource     FundingSource
	ExpiresAt         time.Time
	CapturedAt        *time.Time
	ReleasedAt        *time.Time
	Version           int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ReservationResult struct {
	Account     Account
	Reservation Reservation
	Entry       Entry
	Replayed    bool
}

type ReserveCommand struct {
	PlatformUserID    uuid.UUID
	CheckoutReference string
	OrderID           *string
	RestaurantID      *int64
	Points            int64
	DiscountMinor     int64
	Currency          string
	RewardID          *uuid.UUID
	PolicyVersion     string
	FundingSource     FundingSource
	IdempotencyKey    string
	ExpiresAt         time.Time
	OccurredAt        time.Time
	Metadata          json.RawMessage
	BenefitSnapshot   json.RawMessage
}

type ReservationTransitionCommand struct {
	ReservationID  uuid.UUID
	OrderID        *string
	IdempotencyKey string
	OccurredAt     time.Time
	ActorType      ActorType
	ActorID        *string
	Metadata       json.RawMessage
}

type EntryCursor struct {
	CreatedAt time.Time
	LedgerID  uuid.UUID
}

type EntryPage struct {
	Entries    []Entry
	NextCursor *EntryCursor
}
