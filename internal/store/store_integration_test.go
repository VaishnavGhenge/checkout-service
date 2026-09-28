//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vaishnavghenge/checkout-service/internal/database"
)

func integrationStore(t *testing.T, everyN int) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	_, err = pool.Exec(ctx, `
		TRUNCATE coupons, order_items, orders, cart_items, carts, products RESTART IDENTITY CASCADE;
		INSERT INTO products (id, name, price_cents, inventory) VALUES
			(1, 'Mechanical Keyboard', 8999, 20),
			(2, 'Wireless Mouse', 3499, 30),
			(3, 'USB-C Hub', 4999, 15),
			(4, 'Laptop Stand', 5999, 10),
			(5, 'Limited Edition Keycap', 1299, 2);
		SELECT setval(pg_get_serial_sequence('products', 'id'), 5);
	`)
	if err != nil {
		t.Fatalf("reset test database: %v", err)
	}
	return New(pool, everyN, 10), pool
}

func cartWithItem(t *testing.T, service *Store, productID int64, quantity int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	cart, err := service.CreateCart(ctx)
	if err != nil {
		t.Fatalf("create cart: %v", err)
	}
	if _, err := service.AddCartItem(ctx, cart.ID, productID, quantity); err != nil {
		t.Fatalf("add cart item: %v", err)
	}
	return cart.ID
}

func TestConcurrentIdempotentCheckoutCreatesOneOrder(t *testing.T) {
	service, pool := integrationStore(t, 5)
	cartID := cartWithItem(t, service, 1, 1)

	const attempts = 8
	start := make(chan struct{})
	results := make(chan struct {
		id       uuid.UUID
		replayed bool
		err      error
	}, attempts)
	var waitGroup sync.WaitGroup
	for range attempts {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			order, replayed, err := service.Checkout(context.Background(), cartID, "same-request", "")
			results <- struct {
				id       uuid.UUID
				replayed bool
				err      error
			}{order.ID, replayed, err}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)

	var orderID uuid.UUID
	created := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("checkout attempt failed: %v", result.err)
		}
		if orderID == uuid.Nil {
			orderID = result.id
		} else if result.id != orderID {
			t.Fatalf("idempotent checkouts returned different orders: %s and %s", orderID, result.id)
		}
		if !result.replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("got %d newly-created responses, want 1", created)
	}

	var orders int
	var inventory int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT inventory FROM products WHERE id = 1`).Scan(&inventory); err != nil {
		t.Fatal(err)
	}
	if orders != 1 || inventory != 19 {
		t.Fatalf("orders=%d inventory=%d, want orders=1 inventory=19", orders, inventory)
	}
}

func TestConcurrentCheckoutDoesNotOversell(t *testing.T) {
	service, pool := integrationStore(t, 5)
	carts := []uuid.UUID{
		cartWithItem(t, service, 5, 2),
		cartWithItem(t, service, 5, 2),
	}

	start := make(chan struct{})
	errorsByAttempt := make(chan error, len(carts))
	var waitGroup sync.WaitGroup
	for index, cartID := range carts {
		waitGroup.Add(1)
		go func(index int, cartID uuid.UUID) {
			defer waitGroup.Done()
			<-start
			_, _, err := service.Checkout(context.Background(), cartID, "oversell-"+string(rune('a'+index)), "")
			errorsByAttempt <- err
		}(index, cartID)
	}
	close(start)
	waitGroup.Wait()
	close(errorsByAttempt)

	succeeded, rejected := 0, 0
	for err := range errorsByAttempt {
		if err == nil {
			succeeded++
			continue
		}
		var domainErr *Error
		if errors.As(err, &domainErr) && domainErr.Code == "INSUFFICIENT_INVENTORY" {
			rejected++
			continue
		}
		t.Fatalf("unexpected checkout error: %v", err)
	}
	var inventory int
	if err := pool.QueryRow(context.Background(), `SELECT inventory FROM products WHERE id = 5`).Scan(&inventory); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || rejected != 1 || inventory != 0 {
		t.Fatalf("succeeded=%d rejected=%d inventory=%d, want 1, 1, 0", succeeded, rejected, inventory)
	}
}

func TestCouponGenerationAndRedemptionAreAtomic(t *testing.T) {
	service, _ := integrationStore(t, 1)
	ctx := context.Background()
	seedCart := cartWithItem(t, service, 1, 1)
	if _, _, err := service.Checkout(ctx, seedCart, "seed-order", ""); err != nil {
		t.Fatalf("place milestone order: %v", err)
	}

	const generators = 5
	start := make(chan struct{})
	results := make(chan struct {
		code string
		err  error
	}, generators)
	var waitGroup sync.WaitGroup
	for range generators {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			coupon, err := service.GenerateCoupon(ctx)
			results <- struct {
				code string
				err  error
			}{coupon.Code, err}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)

	var couponCode string
	generated, rejected := 0, 0
	for result := range results {
		if result.err == nil {
			couponCode = result.code
			generated++
			continue
		}
		var domainErr *Error
		if errors.As(result.err, &domainErr) && domainErr.Code == "NO_ELIGIBLE_MILESTONE" {
			rejected++
			continue
		}
		t.Fatalf("unexpected generation error: %v", result.err)
	}
	if generated != 1 || rejected != generators-1 {
		t.Fatalf("generated=%d rejected=%d, want 1 and %d", generated, rejected, generators-1)
	}

	failingCart := cartWithItem(t, service, 5, 3)
	if _, _, err := service.Checkout(ctx, failingCart, "failed-with-coupon", couponCode); err == nil {
		t.Fatal("expected insufficient-inventory checkout to fail")
	}
	report, err := service.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Coupons.Available != 1 || report.Coupons.Redeemed != 0 {
		t.Fatalf("failed checkout consumed coupon: %+v", report.Coupons)
	}

	successfulCart := cartWithItem(t, service, 2, 1)
	order, _, err := service.Checkout(ctx, successfulCart, "coupon-success", couponCode)
	if err != nil {
		t.Fatalf("redeem coupon: %v", err)
	}
	if order.DiscountCents != 350 || order.TotalCents != 3149 {
		t.Fatalf("unexpected discounted totals: discount=%d total=%d", order.DiscountCents, order.TotalCents)
	}

	competingCart := cartWithItem(t, service, 3, 1)
	_, _, err = service.Checkout(ctx, competingCart, "coupon-reuse", couponCode)
	var domainErr *Error
	if !errors.As(err, &domainErr) || domainErr.Code != "COUPON_ALREADY_REDEEMED" {
		t.Fatalf("coupon reuse error=%v, want COUPON_ALREADY_REDEEMED", err)
	}

	report, err = service.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalOrders != 2 || report.Coupons.Generated != 1 || report.Coupons.Redeemed != 1 || report.Coupons.Available != 0 {
		t.Fatalf("unexpected final report: %+v", report)
	}
	if report.GrossRevenueCents != 12498 || report.TotalDiscountsCents != 350 || report.NetRevenueCents != 12148 {
		t.Fatalf("report does not reconcile: %+v", report)
	}
}

func TestOrderKeepsPriceAndNameSnapshot(t *testing.T) {
	service, pool := integrationStore(t, 5)
	ctx := context.Background()
	cartID := cartWithItem(t, service, 1, 2)
	order, _, err := service.Checkout(ctx, cartID, "snapshot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE products SET name = 'Renamed', price_cents = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Order(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Items[0].ProductName != "Mechanical Keyboard" || stored.Items[0].UnitPriceCents != 8999 || stored.TotalCents != 17998 {
		t.Fatalf("order snapshot changed with product: %+v", stored)
	}
}
