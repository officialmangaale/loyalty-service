package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/rewards"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

const testCustomerSecret = "0123456789abcdef0123456789abcdef"

type fakeWalletReader struct {
	account       ledger.Account
	accountErr    error
	page          ledger.EntryPage
	accountUserID uuid.UUID
	entriesUserID uuid.UUID
	limit         int
	cursor        *ledger.EntryCursor
}

func (f *fakeWalletReader) GetAccount(_ context.Context, userID uuid.UUID) (ledger.Account, error) {
	f.accountUserID = userID
	return f.account, f.accountErr
}

func (f *fakeWalletReader) ListEntries(_ context.Context, userID uuid.UUID, limit int, cursor *ledger.EntryCursor) (ledger.EntryPage, error) {
	f.entriesUserID, f.limit, f.cursor = userID, limit, cursor
	return f.page, nil
}

type fakeRewardReader struct{}

func (fakeRewardReader) ListActive(context.Context, *int64, int, time.Time) ([]rewards.Reward, error) {
	return nil, nil
}

func (fakeRewardReader) ResolveActive(context.Context, uuid.UUID, int64, time.Time) (rewards.Reward, rewards.FixedDiscount, error) {
	return rewards.Reward{}, rewards.FixedDiscount{}, rewards.ErrNotFound
}

func TestCustomerReadRoutesRequireJWT(t *testing.T) {
	router := newReadRouter(t, &fakeWalletReader{})
	for _, path := range []string{"/v1/loyalty/wallet", "/v1/loyalty/transactions"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, response.Code)
		}
	}
}

func TestWalletUsesJWTSubjectAndDoesNotCreateMissingAccount(t *testing.T) {
	customerID := uuid.New()
	reader := &fakeWalletReader{account: ledger.Account{
		Status: "ACTIVE", Timezone: "Asia/Kolkata", AvailablePoints: 120,
		PendingPoints: 30, ReservedPoints: 20, DebtPoints: 5,
		LifetimeEarnedPoints: 300, UpdatedAt: time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC),
	}}
	router := newReadRouter(t, reader)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, authenticatedRequest(t, "/v1/loyalty/wallet?user_id="+uuid.NewString(), customerID))
	if response.Code != http.StatusOK || reader.accountUserID != customerID {
		t.Fatalf("status=%d reader customer=%s body=%s", response.Code, reader.accountUserID, response.Body.String())
	}
	var body walletResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.AvailablePoints != 120 || body.Status != "ACTIVE" {
		t.Fatalf("unexpected wallet: %#v", body)
	}

	reader.accountErr = ledger.ErrNotFound
	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, authenticatedRequest(t, "/v1/loyalty/wallet", customerID))
	if missing.Code != http.StatusOK {
		t.Fatalf("missing account status = %d", missing.Code)
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode missing response: %v", err)
	}
	if body.Status != "NOT_ENROLLED" || body.AvailablePoints != 0 {
		t.Fatalf("unexpected missing-account wallet: %#v", body)
	}
}

func TestTransactionsValidatePaginationAndHideInternalKeys(t *testing.T) {
	customerID, entryID, cursorID := uuid.New(), uuid.New(), uuid.New()
	cursorTime := time.Date(2026, 8, 29, 2, 3, 4, 5, time.UTC)
	reader := &fakeWalletReader{page: ledger.EntryPage{
		Entries: []ledger.Entry{{
			LedgerID: entryID, Type: ledger.EntryOrderAvailable, AvailableDelta: 75,
			BusinessKey: "internal-business-key", IdempotencyKey: "internal-idempotency-key",
			ReferenceType: "ORDER", ReferenceID: "order-123",
			FundingSource: ledger.FundingMangaale, PolicyVersion: "earn-v1",
			OccurredAt: cursorTime, CreatedAt: cursorTime,
		}},
		NextCursor: &ledger.EntryCursor{CreatedAt: cursorTime, LedgerID: cursorID},
	}}
	router := newReadRouter(t, reader)
	target := "/v1/loyalty/transactions?limit=25&cursor=" + cursorTime.Format(time.RFC3339Nano) + "%7C" + cursorID.String()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, authenticatedRequest(t, target, customerID))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if reader.entriesUserID != customerID || reader.limit != 25 || reader.cursor == nil || reader.cursor.LedgerID != cursorID {
		t.Fatalf("unexpected reader call: user=%s limit=%d cursor=%#v", reader.entriesUserID, reader.limit, reader.cursor)
	}
	for _, forbidden := range []string{"business_key", "idempotency_key", "metadata", "account_id", "platform_user_id"} {
		if strings.Contains(response.Body.String(), `"`+forbidden+`"`) {
			t.Fatalf("response exposed internal field %q: %s", forbidden, response.Body.String())
		}
	}

	for _, query := range []string{"limit=0", "limit=101", "limit=nope", "cursor=bad"} {
		bad := httptest.NewRecorder()
		router.ServeHTTP(bad, authenticatedRequest(t, "/v1/loyalty/transactions?"+query, customerID))
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want 400", query, bad.Code)
		}
	}
}

func TestReadinessFailureIsGeneric(t *testing.T) {
	router, err := NewRouter(readConfig(), discardLogger(), Dependencies{
		WalletReader: &fakeWalletReader{},
		RewardReader: fakeRewardReader{},
		Readiness:    func(context.Context) error { return errors.New("database hostname and secret details") },
	})
	if err != nil {
		t.Fatalf("create router: %v", err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "database") {
		t.Fatalf("unexpected readiness response: %d %s", response.Code, response.Body.String())
	}
}

func newReadRouter(t *testing.T, reader WalletReader) http.Handler {
	t.Helper()
	router, err := NewRouter(readConfig(), discardLogger(), Dependencies{WalletReader: reader, RewardReader: fakeRewardReader{}})
	if err != nil {
		t.Fatalf("create router: %v", err)
	}
	return router
}

func readConfig() config.Config {
	return config.Config{Environment: "test", LogLevel: "ERROR", CustomerJWTSecret: testCustomerSecret,
		Features: config.FeatureFlags{ReadAPI: true}}
}

func authenticatedRequest(t *testing.T, target string, userID uuid.UUID) *http.Request {
	t.Helper()
	now := time.Now().UTC()
	claims := jwt.MapClaims{"sub": userID.String(), "user_id": userID.String(), "role": "customer",
		"type": "customer", "user_type": "customer", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testCustomerSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}
