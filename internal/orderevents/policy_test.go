package orderevents

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEarningPolicyUsesIntegerArithmetic(t *testing.T) {
	policy, err := decodePolicy("earn-v1", []byte(`{
		"basis":"SUBTOTAL_MINUS_DISCOUNTS",
		"points_numerator":1,
		"amount_millis_denominator":1000,
		"expiry_days":180,
		"expiry_anchor":"ORDER_PLACED",
		"allowed_order_types":["DELIVERY","DINE_IN"],
		"allow_qr":true,
		"funding_source":"MANGAALE"
	}`))
	if err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	event := validPlacedEvent()
	event.Data.SubtotalMillis = 499999
	event.Data.CouponDiscountMillis = 50000
	event.Data.OfferDiscountMillis = 10000
	points, expiresAt, eligible, err := policy.Evaluate(event)
	if err != nil || !eligible {
		t.Fatalf("evaluate: eligible=%v err=%v", eligible, err)
	}
	if points != 439 {
		t.Fatalf("points = %d, want floor(439999/1000)=439", points)
	}
	wantExpiry := event.OccurredAt.AddDate(0, 0, 180)
	if expiresAt == nil || !expiresAt.Equal(wantExpiry) {
		t.Fatalf("expiry = %v, want %v", expiresAt, wantExpiry)
	}
}

func TestPolicyHasNoImplicitEligibilityDefaults(t *testing.T) {
	_, err := decodePolicy("earn-v1", []byte(`{"points_numerator":1}`))
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("incomplete policy error = %v", err)
	}
	_, err = decodePolicy("earn-v1", []byte(`{
		"basis":"PAYABLE","points_numerator":1,"amount_millis_denominator":1000,
		"expiry_days":180,"expiry_anchor":"ORDER_PLACED","allowed_order_types":["DELIVERY"],
		"allow_qr":false,"funding_source":"MANGAALE","unknown_rule":true
	}`))
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unknown policy field error = %v", err)
	}
}

func TestEnvelopeValidationRejectsIdentityAndLifecycleMismatch(t *testing.T) {
	now := time.Now().UTC()
	event := validPlacedEvent()
	if err := event.Validate(now); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	tests := []Envelope{event, event, event, event}
	tests[0].Data.PlatformUserID = stringPointer("not-a-uuid")
	tests[1].Data.OrderStatus = "completed"
	tests[2].AggregateID = "999"
	tests[3].Data.Currency = "USD"
	for index, invalid := range tests {
		if err := invalid.Validate(now); err == nil {
			t.Fatalf("invalid event %d accepted", index)
		}
	}
}

func validPlacedEvent() Envelope {
	userID := uuid.NewString()
	occurredAt := time.Now().UTC().Add(-time.Minute)
	return Envelope{
		EventID: uuid.New(), EventType: EventOrderPlaced, SchemaVersion: SchemaVersion,
		OccurredAt: occurredAt, AggregateType: "order", AggregateID: "123", Sequence: 1,
		Data: OrderData{
			OrderID: 123, RestaurantID: 42, PlatformUserID: &userID, OrderType: "DELIVERY",
			OrderStatus: "pending", PaymentStatus: "pending", PaymentMethod: "cash",
			Currency: "INR", ActorType: "system", Source: "restaurant_service",
			SubtotalMillis: 500000, PayableAmountMillis: 500000,
		},
	}
}

func stringPointer(value string) *string { return &value }
