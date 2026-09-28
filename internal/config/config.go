package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	HTTPAddr              string
	DatabaseURL           string
	CouponEveryNOrders    int
	CouponDiscountPercent int
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    envOrDefault("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	var err error
	cfg.CouponEveryNOrders, err = positiveIntEnv("COUPON_EVERY_N_ORDERS", 5)
	if err != nil {
		return Config{}, err
	}
	cfg.CouponDiscountPercent, err = positiveIntEnv("COUPON_DISCOUNT_PERCENT", 10)
	if err != nil {
		return Config{}, err
	}
	if cfg.CouponDiscountPercent > 100 {
		return Config{}, fmt.Errorf("COUPON_DISCOUNT_PERCENT must be at most 100")
	}
	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func positiveIntEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}
