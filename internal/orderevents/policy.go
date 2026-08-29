package orderevents

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
)

const policyTypeOrderEarning = "ORDER_EARNING"

type EarningPolicy struct {
	Version                 string
	Basis                   string   `json:"basis"`
	PointsNumerator         int64    `json:"points_numerator"`
	AmountMillisDenominator int64    `json:"amount_millis_denominator"`
	ExpiryDays              int      `json:"expiry_days"`
	ExpiryAnchor            string   `json:"expiry_anchor"`
	AllowedOrderTypes       []string `json:"allowed_order_types"`
	AllowQR                 bool     `json:"allow_qr"`
	FundingSource           string   `json:"funding_source"`
}

func decodePolicy(version string, raw []byte) (EarningPolicy, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	policy := EarningPolicy{Version: version}
	if err := decoder.Decode(&policy); err != nil {
		return EarningPolicy{}, ErrInvalidEvent
	}
	if strings.TrimSpace(version) == "" || policy.PointsNumerator <= 0 || policy.AmountMillisDenominator <= 0 ||
		policy.ExpiryDays < 1 || policy.ExpiryDays > 3650 || policy.ExpiryAnchor != "ORDER_PLACED" ||
		(policy.Basis != "PAYABLE" && policy.Basis != "SUBTOTAL_MINUS_DISCOUNTS") ||
		len(policy.AllowedOrderTypes) == 0 {
		return EarningPolicy{}, ErrInvalidEvent
	}
	switch ledger.FundingSource(policy.FundingSource) {
	case ledger.FundingMangaale, ledger.FundingRestaurant, ledger.FundingShared:
	default:
		return EarningPolicy{}, ErrInvalidEvent
	}
	return policy, nil
}

func (p EarningPolicy) Evaluate(event Envelope) (int64, *time.Time, bool, error) {
	if event.Data.IsQrunch && !p.AllowQR {
		return 0, nil, false, nil
	}
	allowed := false
	for _, orderType := range p.AllowedOrderTypes {
		if strings.EqualFold(strings.TrimSpace(orderType), event.Data.OrderType) {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, nil, false, nil
	}
	base := event.Data.PayableAmountMillis
	if p.Basis == "SUBTOTAL_MINUS_DISCOUNTS" {
		base = event.Data.SubtotalMillis - event.Data.CouponDiscountMillis - event.Data.OfferDiscountMillis
		if base < 0 {
			base = 0
		}
	}
	pointsBig := new(big.Int).Mul(big.NewInt(base), big.NewInt(p.PointsNumerator))
	pointsBig.Div(pointsBig, big.NewInt(p.AmountMillisDenominator))
	if !pointsBig.IsInt64() {
		return 0, nil, false, ErrInvalidEvent
	}
	points := pointsBig.Int64()
	if points <= 0 {
		return 0, nil, false, nil
	}
	expiresAt := event.OccurredAt.UTC().AddDate(0, 0, p.ExpiryDays)
	return points, &expiresAt, true, nil
}
