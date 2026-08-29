package rewards

import (
	"errors"
	"testing"
)

func TestDecodeFixedDiscountIsStrictAndIntegerOnly(t *testing.T) {
	effect, err := decodeFixedDiscount([]byte(`{"discount_minor":1500,"currency":"inr"}`))
	if err != nil || effect.DiscountMinor != 1500 || effect.Currency != "INR" {
		t.Fatalf("effect=%#v err=%v", effect, err)
	}
	for _, raw := range []string{
		`{"discount_minor":0,"currency":"INR"}`,
		`{"discount_minor":10.5,"currency":"INR"}`,
		`{"discount_minor":100,"currency":"RUPEES"}`,
		`{"discount_minor":100,"currency":"INR","client_amount":999}`,
	} {
		if _, err := decodeFixedDiscount([]byte(raw)); !errors.Is(err, ErrUnsupportedEffect) {
			t.Fatalf("invalid effect %s error = %v", raw, err)
		}
	}
}
