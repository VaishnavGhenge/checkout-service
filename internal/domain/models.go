package domain

import (
	"time"

	"github.com/google/uuid"
)

const Currency = "USD"

type Product struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	Inventory      int    `json:"inventory"`
	Currency       string `json:"currency"`
}

type CartItem struct {
	ProductID      int64  `json:"product_id"`
	ProductName    string `json:"product_name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	LineTotalCents int64  `json:"line_total_cents"`
}

type Cart struct {
	ID            uuid.UUID  `json:"id"`
	Status        string     `json:"status"`
	Items         []CartItem `json:"items"`
	SubtotalCents int64      `json:"subtotal_cents"`
	Currency      string     `json:"currency"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type OrderItem struct {
	ProductID      int64  `json:"product_id"`
	ProductName    string `json:"product_name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	LineTotalCents int64  `json:"line_total_cents"`
}

type Order struct {
	ID             uuid.UUID   `json:"id"`
	CartID         uuid.UUID   `json:"cart_id"`
	Items          []OrderItem `json:"items"`
	SubtotalCents  int64       `json:"subtotal_cents"`
	DiscountCents  int64       `json:"discount_cents"`
	TotalCents     int64       `json:"total_cents"`
	Currency       string      `json:"currency"`
	CouponCode     *string     `json:"coupon_code,omitempty"`
	IdempotencyKey string      `json:"idempotency_key"`
	CreatedAt      time.Time   `json:"created_at"`
}

type Coupon struct {
	ID                  uuid.UUID  `json:"id"`
	Code                string     `json:"code"`
	MilestoneOrderCount int64      `json:"milestone_order_count"`
	DiscountPercent     int        `json:"discount_percent"`
	Status              string     `json:"status"`
	RedeemedOrderID     *uuid.UUID `json:"redeemed_order_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	RedeemedAt          *time.Time `json:"redeemed_at,omitempty"`
}

type ProductQuantity struct {
	ProductID   int64  `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int64  `json:"quantity"`
}

type CouponSummary struct {
	Generated int64 `json:"generated"`
	Available int64 `json:"available"`
	Redeemed  int64 `json:"redeemed"`
}

type Report struct {
	PurchasedQuantityByProduct []ProductQuantity `json:"purchased_quantity_by_product"`
	GrossRevenueCents          int64             `json:"gross_revenue_cents"`
	TotalDiscountsCents        int64             `json:"total_discounts_cents"`
	NetRevenueCents            int64             `json:"net_revenue_cents"`
	Currency                   string            `json:"currency"`
	Coupons                    CouponSummary     `json:"coupons"`
	TotalOrders                int64             `json:"total_orders"`
}
