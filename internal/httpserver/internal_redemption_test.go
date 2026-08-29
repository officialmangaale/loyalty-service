package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/rewards"
	"github.com/google/uuid"
)

type redemptionRewardReader struct{ reward rewards.Reward }

func (r redemptionRewardReader) ListActive(context.Context, *int64, int, time.Time) ([]rewards.Reward, error) {
	return []rewards.Reward{r.reward}, nil
}

func (r redemptionRewardReader) ResolveActive(context.Context, uuid.UUID, int64, time.Time) (rewards.Reward, rewards.FixedDiscount, error) {
	return r.reward, rewards.FixedDiscount{DiscountMinor: 1000, Currency: "INR"}, nil
}

type fakeReservationWriter struct {
	reserve  ledger.ReserveCommand
	capture  ledger.ReservationTransitionCommand
	existing *ledger.Reservation
}

func (f *fakeReservationWriter) FindReservationByIdempotency(_ context.Context, _ uuid.UUID, _ string) (ledger.Reservation, bool, error) {
	if f.existing == nil {
		return ledger.Reservation{}, false, nil
	}
	return *f.existing, true, nil
}

func (f *fakeReservationWriter) ReservePoints(_ context.Context, command ledger.ReserveCommand) (ledger.ReservationResult, error) {
	f.reserve = command
	return ledger.ReservationResult{Reservation: ledger.Reservation{
		ReservationID: uuid.New(), RestaurantID: command.RestaurantID, RewardID: command.RewardID,
		Points: command.Points, DiscountMinor: command.DiscountMinor, Currency: command.Currency,
		Status: "ACTIVE", FundingSource: command.FundingSource, PolicyVersion: command.PolicyVersion,
		ExpiresAt: command.ExpiresAt,
	}}, nil
}

func (f *fakeReservationWriter) ReleaseReservation(_ context.Context, command ledger.ReservationTransitionCommand) (ledger.ReservationResult, error) {
	return ledger.ReservationResult{Reservation: ledger.Reservation{ReservationID: command.ReservationID, Status: "RELEASED"}}, nil
}

func (f *fakeReservationWriter) CaptureReservation(_ context.Context, command ledger.ReservationTransitionCommand) (ledger.ReservationResult, error) {
	f.capture = command
	return ledger.ReservationResult{Reservation: ledger.Reservation{ReservationID: command.ReservationID, OrderID: command.OrderID, Status: "CAPTURED"}}, nil
}

func TestInternalReserveUsesCatalogValuesAndServiceAuth(t *testing.T) {
	rewardID := uuid.New()
	reward := rewards.Reward{
		RewardID: rewardID, PointsCost: 100, Effect: json.RawMessage(`{"discount_minor":1000,"currency":"INR"}`),
		FundingSource: ledger.FundingMangaale, PolicyVersion: "reward-v1",
	}
	writer := &fakeReservationWriter{}
	router := internalRouter(t, redemptionRewardReader{reward: reward}, writer)
	body := `{"platform_user_id":"` + uuid.NewString() + `","reward_id":"` + rewardID.String() +
		`","restaurant_id":42,"checkout_reference":"checkout-123","client_discount_minor":999999}`

	invalid := httptest.NewRequest(http.MethodPost, "/internal/v1/loyalty/reservations", strings.NewReader(body))
	invalid.Header.Set("Authorization", "Bearer "+testCustomerSecret)
	invalid.Header.Set("Idempotency-Key", "checkout-123")
	invalidResponse := httptest.NewRecorder()
	router.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("client financial field status = %d", invalidResponse.Code)
	}

	body = `{"platform_user_id":"` + uuid.NewString() + `","reward_id":"` + rewardID.String() +
		`","restaurant_id":42,"checkout_reference":"checkout-123"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/loyalty/reservations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testCustomerSecret)
	request.Header.Set("Idempotency-Key", "checkout-123")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if writer.reserve.Points != 100 || writer.reserve.DiscountMinor != 1000 || writer.reserve.Currency != "INR" {
		t.Fatalf("reservation did not use catalog values: %#v", writer.reserve)
	}

	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/loyalty/reservations", strings.NewReader(body))
	unauthorized.Header.Set("Idempotency-Key", "checkout-123")
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResponse.Code)
	}
}

func TestInternalReserveReplaysBeforeCatalogResolution(t *testing.T) {
	userID := uuid.New()
	rewardID := uuid.New()
	restaurantID := int64(42)
	existing := ledger.Reservation{
		ReservationID: uuid.New(), CheckoutReference: "checkout-123", RewardID: &rewardID,
		RestaurantID: &restaurantID, Status: "CAPTURED", Points: 100,
		DiscountMinor: 1000, Currency: "INR", FundingSource: ledger.FundingMangaale,
		PolicyVersion: "reward-v1", ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	writer := &fakeReservationWriter{existing: &existing}
	// A zero-value reader would return unsupported catalog data if called. The
	// exact retry must instead replay the already accepted reservation.
	router := internalRouter(t, redemptionRewardReader{}, writer)
	body := `{"platform_user_id":"` + userID.String() + `","reward_id":"` + rewardID.String() +
		`","restaurant_id":42,"checkout_reference":"checkout-123"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/loyalty/reservations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testCustomerSecret)
	request.Header.Set("Idempotency-Key", "checkout-123")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"replayed":true`) || writer.reserve.PlatformUserID != uuid.Nil {
		t.Fatalf("reservation was not replayed before catalog resolution: %s", response.Body.String())
	}

	conflictBody := `{"platform_user_id":"` + userID.String() + `","reward_id":"` + rewardID.String() +
		`","restaurant_id":42,"checkout_reference":"different-checkout"}`
	conflict := httptest.NewRequest(http.MethodPost, "/internal/v1/loyalty/reservations", strings.NewReader(conflictBody))
	conflict.Header.Set("Authorization", "Bearer "+testCustomerSecret)
	conflict.Header.Set("Idempotency-Key", "checkout-123")
	conflictResponse := httptest.NewRecorder()
	router.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("mismatched replay status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
}

func internalRouter(t *testing.T, reader RewardReader, writer ReservationWriter) http.Handler {
	t.Helper()
	router, err := NewRouter(config.Config{
		Environment: "test", LogLevel: "ERROR", InternalServiceToken: testCustomerSecret,
		ReservationTTL: time.Minute, Features: config.FeatureFlags{InternalAPI: true, Redemption: true},
	}, discardLogger(), Dependencies{RewardReader: reader, ReservationWriter: writer})
	if err != nil {
		t.Fatalf("create router: %v", err)
	}
	return router
}
