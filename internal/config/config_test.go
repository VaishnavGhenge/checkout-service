package config

import "testing"

func TestLoadValidatesCouponConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("COUPON_EVERY_N_ORDERS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected zero milestone interval to be rejected")
	}

	t.Setenv("COUPON_EVERY_N_ORDERS", "5")
	t.Setenv("COUPON_DISCOUNT_PERCENT", "101")
	if _, err := Load(); err == nil {
		t.Fatal("expected discount over 100 percent to be rejected")
	}
}
