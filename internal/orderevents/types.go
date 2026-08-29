package orderevents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const SchemaVersion = 1

const (
	EventOrderPlaced    = "order.placed.v1"
	EventOrderCompleted = "order.completed.v1"
	EventOrderCancelled = "order.cancelled.v1"
	EventOrderRejected  = "order.rejected.v1"
)

var (
	ErrInvalidEvent     = errors.New("invalid order event")
	ErrEventConflict    = errors.New("order event identity conflict")
	ErrNoActivePolicy   = errors.New("no active approved earning policy")
	ErrEarningDisabled  = errors.New("order earning is globally disabled")
	ErrUnsupportedEvent = errors.New("unsupported order event")
)

type Envelope struct {
	EventID       uuid.UUID `json:"event_id"`
	EventType     string    `json:"event_type"`
	SchemaVersion int       `json:"schema_version"`
	OccurredAt    time.Time `json:"occurred_at"`
	AggregateType string    `json:"aggregate_type"`
	AggregateID   string    `json:"aggregate_id"`
	Sequence      int64     `json:"sequence"`
	Data          OrderData `json:"data"`
}

func DecodeStrict(payload []byte) (Envelope, error) {
	if len(payload) == 0 || len(payload) > 64*1024 {
		return Envelope{}, ErrInvalidEvent
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var event Envelope
	if err := decoder.Decode(&event); err != nil {
		return Envelope{}, ErrInvalidEvent
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Envelope{}, ErrInvalidEvent
	}
	return event, nil
}

type OrderData struct {
	OrderID              int64   `json:"order_id"`
	RestaurantID         int64   `json:"restaurant_id"`
	PlatformUserID       *string `json:"platform_user_id"`
	OrderType            string  `json:"order_type"`
	IsQrunch             bool    `json:"is_qrunch"`
	OrderStatus          string  `json:"order_status"`
	PreviousStatus       string  `json:"previous_status,omitempty"`
	PaymentStatus        string  `json:"payment_status"`
	PaymentMethod        string  `json:"payment_method"`
	Currency             string  `json:"currency"`
	ActorType            string  `json:"actor_type"`
	Source               string  `json:"source"`
	SubtotalMillis       int64   `json:"subtotal_millis"`
	TaxMillis            int64   `json:"tax_millis"`
	DeliveryFeeMillis    int64   `json:"delivery_fee_millis"`
	TipMillis            int64   `json:"tip_millis"`
	CouponDiscountMillis int64   `json:"coupon_discount_millis"`
	OfferDiscountMillis  int64   `json:"offer_discount_millis"`
	PlatformFeeMillis    int64   `json:"platform_fee_millis"`
	PayableAmountMillis  int64   `json:"payable_amount_millis"`
}

func (e Envelope) Validate(now time.Time) error {
	if e.EventID == uuid.Nil || e.SchemaVersion != SchemaVersion || e.AggregateType != "order" ||
		e.Sequence <= 0 || e.OccurredAt.IsZero() || e.OccurredAt.After(now.UTC().Add(5*time.Minute)) ||
		e.Data.OrderID <= 0 || e.Data.RestaurantID <= 0 || e.AggregateID != strconv.FormatInt(e.Data.OrderID, 10) {
		return ErrInvalidEvent
	}
	if strings.ToUpper(e.Data.Currency) != "INR" || e.Data.Source != "restaurant_service" {
		return ErrInvalidEvent
	}
	for _, amount := range []int64{e.Data.SubtotalMillis, e.Data.TaxMillis, e.Data.DeliveryFeeMillis,
		e.Data.TipMillis, e.Data.CouponDiscountMillis, e.Data.OfferDiscountMillis,
		e.Data.PlatformFeeMillis, e.Data.PayableAmountMillis} {
		if amount < 0 {
			return ErrInvalidEvent
		}
	}
	if e.Data.PlatformUserID != nil {
		parsed, err := uuid.Parse(strings.TrimSpace(*e.Data.PlatformUserID))
		if err != nil || parsed == uuid.Nil {
			return ErrInvalidEvent
		}
	}
	wantStatus := map[string]string{
		EventOrderPlaced: "pending", EventOrderCompleted: "completed",
		EventOrderCancelled: "cancelled", EventOrderRejected: "rejected",
	}[e.EventType]
	if wantStatus == "" {
		return fmt.Errorf("%w: %s", ErrUnsupportedEvent, e.EventType)
	}
	if strings.ToLower(strings.TrimSpace(e.Data.OrderStatus)) != wantStatus {
		return ErrInvalidEvent
	}
	return nil
}

func (e Envelope) PlatformUserUUID() (uuid.UUID, bool) {
	if e.Data.PlatformUserID == nil {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(*e.Data.PlatformUserID)
	return parsed, err == nil && parsed != uuid.Nil
}
