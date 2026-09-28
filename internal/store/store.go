package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vaishnavghenge/checkout-service/internal/domain"
)

type Store struct {
	pool                  *pgxpool.Pool
	couponEveryNOrders    int
	couponDiscountPercent int
}

func New(pool *pgxpool.Pool, couponEveryNOrders, couponDiscountPercent int) *Store {
	return &Store{
		pool:                  pool,
		couponEveryNOrders:    couponEveryNOrders,
		couponDiscountPercent: couponDiscountPercent,
	}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Products(ctx context.Context) ([]domain.Product, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, price_cents, inventory FROM products ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()

	products := make([]domain.Product, 0)
	for rows.Next() {
		var product domain.Product
		if err := rows.Scan(&product.ID, &product.Name, &product.UnitPriceCents, &product.Inventory); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		product.Currency = domain.Currency
		products = append(products, product)
	}
	return products, rows.Err()
}

func (s *Store) CreateCart(ctx context.Context) (domain.Cart, error) {
	cart := domain.Cart{ID: uuid.New(), Status: "open", Items: []domain.CartItem{}, Currency: domain.Currency}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO carts (id) VALUES ($1)
		RETURNING created_at, updated_at`, cart.ID).Scan(&cart.CreatedAt, &cart.UpdatedAt)
	if err != nil {
		return domain.Cart{}, fmt.Errorf("create cart: %w", err)
	}
	return cart, nil
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) Cart(ctx context.Context, id uuid.UUID) (domain.Cart, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Cart{}, fmt.Errorf("begin cart read: %w", err)
	}
	defer tx.Rollback(ctx)

	cart, err := readCart(ctx, tx, id)
	if err != nil {
		return domain.Cart{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Cart{}, fmt.Errorf("commit cart read: %w", err)
	}
	return cart, nil
}

func readCart(ctx context.Context, q querier, id uuid.UUID) (domain.Cart, error) {
	var cart domain.Cart
	err := q.QueryRow(ctx, `
		SELECT c.id, c.status, c.created_at, c.updated_at, o.id
		FROM carts c
		LEFT JOIN orders o ON o.cart_id = c.id
		WHERE c.id = $1`, id).
		Scan(&cart.ID, &cart.Status, &cart.CreatedAt, &cart.UpdatedAt, &cart.OrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Cart{}, notFound("cart")
	}
	if err != nil {
		return domain.Cart{}, fmt.Errorf("get cart: %w", err)
	}

	rows, err := q.Query(ctx, `
		SELECT p.id, p.name, ci.quantity, p.price_cents, p.price_cents * ci.quantity
		FROM cart_items ci
		JOIN products p ON p.id = ci.product_id
		WHERE ci.cart_id = $1
		ORDER BY p.id`, id)
	if err != nil {
		return domain.Cart{}, fmt.Errorf("get cart items: %w", err)
	}
	defer rows.Close()

	cart.Items = make([]domain.CartItem, 0)
	for rows.Next() {
		var item domain.CartItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPriceCents, &item.LineTotalCents); err != nil {
			return domain.Cart{}, fmt.Errorf("scan cart item: %w", err)
		}
		cart.SubtotalCents += item.LineTotalCents
		cart.Items = append(cart.Items, item)
	}
	if err := rows.Err(); err != nil {
		return domain.Cart{}, fmt.Errorf("iterate cart items: %w", err)
	}
	cart.Currency = domain.Currency
	return cart, nil
}

func (s *Store) AddCartItem(ctx context.Context, cartID uuid.UUID, productID int64, quantity int) (domain.Cart, error) {
	return s.mutateCartItem(ctx, cartID, productID, quantity, "add")
}

func (s *Store) UpdateCartItem(ctx context.Context, cartID uuid.UUID, productID int64, quantity int) (domain.Cart, error) {
	return s.mutateCartItem(ctx, cartID, productID, quantity, "update")
}

func (s *Store) mutateCartItem(ctx context.Context, cartID uuid.UUID, productID int64, quantity int, operation string) (domain.Cart, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Cart{}, fmt.Errorf("begin cart mutation: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := lockOpenCart(ctx, tx, cartID); err != nil {
		return domain.Cart{}, err
	}
	var inventory int
	err = tx.QueryRow(ctx, `SELECT inventory FROM products WHERE id = $1`, productID).Scan(&inventory)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Cart{}, notFound("product")
	}
	if err != nil {
		return domain.Cart{}, fmt.Errorf("check product: %w", err)
	}
	if quantity > inventory {
		return domain.Cart{}, newError("INSUFFICIENT_INVENTORY", fmt.Sprintf("product %d has %d units available but %d were requested", productID, inventory, quantity))
	}

	if operation == "add" {
		command, err := tx.Exec(ctx, `
			INSERT INTO cart_items (cart_id, product_id, quantity) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, cartID, productID, quantity)
		if err != nil {
			return domain.Cart{}, fmt.Errorf("add cart item: %w", err)
		}
		if command.RowsAffected() == 0 {
			return domain.Cart{}, newError("ITEM_ALREADY_EXISTS", "product is already in the cart; use PUT to change its quantity")
		}
	} else {
		command, err := tx.Exec(ctx, `UPDATE cart_items SET quantity = $3 WHERE cart_id = $1 AND product_id = $2`, cartID, productID, quantity)
		if err != nil {
			return domain.Cart{}, fmt.Errorf("update cart item: %w", err)
		}
		if command.RowsAffected() == 0 {
			return domain.Cart{}, notFound("cart item")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE carts SET updated_at = now() WHERE id = $1`, cartID); err != nil {
		return domain.Cart{}, fmt.Errorf("touch cart: %w", err)
	}

	cart, err := readCart(ctx, tx, cartID)
	if err != nil {
		return domain.Cart{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Cart{}, fmt.Errorf("commit cart mutation: %w", err)
	}
	return cart, nil
}

func (s *Store) RemoveCartItem(ctx context.Context, cartID uuid.UUID, productID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin remove cart item: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockOpenCart(ctx, tx, cartID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_id = $1 AND product_id = $2`, cartID, productID)
	if err != nil {
		return fmt.Errorf("remove cart item: %w", err)
	}
	if command.RowsAffected() == 0 {
		return notFound("cart item")
	}
	if _, err := tx.Exec(ctx, `UPDATE carts SET updated_at = now() WHERE id = $1`, cartID); err != nil {
		return fmt.Errorf("touch cart: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit remove cart item: %w", err)
	}
	return nil
}

func lockOpenCart(ctx context.Context, tx pgx.Tx, cartID uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM carts WHERE id = $1 FOR UPDATE`, cartID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("cart")
	}
	if err != nil {
		return fmt.Errorf("lock cart: %w", err)
	}
	if status != "open" {
		return newError("CART_ALREADY_CHECKED_OUT", "checked-out carts cannot be changed")
	}
	return nil
}

func (s *Store) Checkout(ctx context.Context, cartID uuid.UUID, idempotencyKey, couponCode string) (domain.Order, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	couponCode = normalizeCouponCode(couponCode)
	if order, found, err := s.replay(ctx, cartID, idempotencyKey, couponCode); err != nil || found {
		return order, found, err
	}

	order, err := s.checkoutTransaction(ctx, cartID, idempotencyKey, couponCode)
	if err == nil {
		return order, false, nil
	}
	// Both races below mean another request committed first. If it used this
	// key, the answer is its order (or a key-reuse conflict), not our error.
	var domainErr *Error
	var pgErr *pgconn.PgError
	lostCartRace := errors.As(err, &domainErr) && domainErr.Code == "CART_ALREADY_CHECKED_OUT"
	lostKeyRace := errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "orders_idempotency_key_key"
	if lostCartRace || lostKeyRace {
		if order, found, lookupErr := s.replay(ctx, cartID, idempotencyKey, couponCode); lookupErr != nil || found {
			return order, found, lookupErr
		}
	}
	return domain.Order{}, false, err
}

// replay returns the committed order for key when the request matches it, and
// IDEMPOTENCY_KEY_REUSED when the key belongs to a different cart or coupon.
func (s *Store) replay(ctx context.Context, cartID uuid.UUID, idempotencyKey, couponCode string) (domain.Order, bool, error) {
	existing, found, err := s.orderByIdempotencyKey(ctx, idempotencyKey)
	if err != nil || !found {
		return domain.Order{}, false, err
	}
	if existing.CartID != cartID {
		return domain.Order{}, false, newError("IDEMPOTENCY_KEY_REUSED", "idempotency key was already used for a different cart")
	}
	if !sameCoupon(existing.CouponCode, couponCode) {
		return domain.Order{}, false, newError("IDEMPOTENCY_KEY_REUSED", "idempotency key was already used with a different coupon")
	}
	return existing, true, nil
}

func sameCoupon(existing *string, requested string) bool {
	if existing == nil {
		return requested == ""
	}
	return *existing == requested
}

// Generated codes are upper case; accepting any case avoids rejecting a code
// a customer retyped.
func normalizeCouponCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (s *Store) checkoutTransaction(ctx context.Context, cartID uuid.UUID, idempotencyKey, couponCode string) (domain.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("begin checkout: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := lockOpenCart(ctx, tx, cartID); err != nil {
		return domain.Order{}, err
	}

	rows, err := tx.Query(ctx, `
		SELECT p.id, p.name, p.price_cents, p.inventory, ci.quantity
		FROM cart_items ci
		JOIN products p ON p.id = ci.product_id
		WHERE ci.cart_id = $1
		ORDER BY p.id
		FOR UPDATE OF p`, cartID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("lock checkout products: %w", err)
	}
	type checkoutItem struct {
		id, priceCents int64
		name           string
		inventory      int
		quantity       int
	}
	items := make([]checkoutItem, 0)
	for rows.Next() {
		var item checkoutItem
		if err := rows.Scan(&item.id, &item.name, &item.priceCents, &item.inventory, &item.quantity); err != nil {
			rows.Close()
			return domain.Order{}, fmt.Errorf("scan checkout item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.Order{}, fmt.Errorf("iterate checkout items: %w", err)
	}
	rows.Close()
	if len(items) == 0 {
		return domain.Order{}, newError("EMPTY_CART", "an empty cart cannot be checked out")
	}

	order := domain.Order{
		ID:             uuid.New(),
		CartID:         cartID,
		Items:          make([]domain.OrderItem, 0, len(items)),
		Currency:       domain.Currency,
		IdempotencyKey: idempotencyKey,
	}
	for _, item := range items {
		if item.quantity > item.inventory {
			return domain.Order{}, newError("INSUFFICIENT_INVENTORY", fmt.Sprintf("product %d has %d units available but %d were requested", item.id, item.inventory, item.quantity))
		}
		lineTotal := item.priceCents * int64(item.quantity)
		order.SubtotalCents += lineTotal
		order.Items = append(order.Items, domain.OrderItem{
			ProductID: item.id, ProductName: item.name, Quantity: item.quantity,
			UnitPriceCents: item.priceCents, LineTotalCents: lineTotal,
		})
	}

	var couponID *uuid.UUID
	if couponCode != "" {
		var id uuid.UUID
		var percent int
		var redeemedOrderID *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id, discount_percent, redeemed_order_id
			FROM coupons WHERE code = $1 FOR UPDATE`, couponCode).Scan(&id, &percent, &redeemedOrderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Order{}, newError("COUPON_NOT_FOUND", "coupon does not exist")
		}
		if err != nil {
			return domain.Order{}, fmt.Errorf("lock coupon: %w", err)
		}
		if redeemedOrderID != nil {
			return domain.Order{}, newError("COUPON_ALREADY_REDEEMED", "coupon has already been redeemed")
		}
		couponID = &id
		order.CouponCode = &couponCode
		order.DiscountCents = percentageDiscount(order.SubtotalCents, percent)
	}
	order.TotalCents = order.SubtotalCents - order.DiscountCents

	for _, item := range items {
		if _, err := tx.Exec(ctx, `UPDATE products SET inventory = inventory - $2, updated_at = now() WHERE id = $1`, item.id, item.quantity); err != nil {
			return domain.Order{}, fmt.Errorf("decrement inventory: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (id, cart_id, idempotency_key, subtotal_cents, discount_cents, total_cents)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`, order.ID, order.CartID, order.IdempotencyKey, order.SubtotalCents, order.DiscountCents, order.TotalCents).
		Scan(&order.CreatedAt); err != nil {
		return domain.Order{}, fmt.Errorf("insert order: %w", err)
	}
	for _, item := range order.Items {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, product_id, product_name, unit_price_cents, quantity, line_total_cents)
			VALUES ($1, $2, $3, $4, $5, $6)`, order.ID, item.ProductID, item.ProductName, item.UnitPriceCents, item.Quantity, item.LineTotalCents)
		if err != nil {
			return domain.Order{}, fmt.Errorf("insert order item: %w", err)
		}
	}
	if couponID != nil {
		if _, err := tx.Exec(ctx, `UPDATE coupons SET redeemed_order_id = $2, redeemed_at = now() WHERE id = $1`, *couponID, order.ID); err != nil {
			return domain.Order{}, fmt.Errorf("redeem coupon: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE carts SET status = 'checked_out', updated_at = now() WHERE id = $1`, cartID); err != nil {
		return domain.Order{}, fmt.Errorf("close cart: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, fmt.Errorf("commit checkout: %w", err)
	}
	return order, nil
}

func percentageDiscount(subtotal int64, percent int) int64 {
	discount := (subtotal*int64(percent) + 50) / 100
	if discount > subtotal {
		return subtotal
	}
	return discount
}

func (s *Store) Order(ctx context.Context, id uuid.UUID) (domain.Order, error) {
	order, err := readOrder(ctx, s.pool, `o.id = $1`, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, notFound("order")
	}
	return order, err
}

func (s *Store) orderByIdempotencyKey(ctx context.Context, key string) (domain.Order, bool, error) {
	order, err := readOrder(ctx, s.pool, `o.idempotency_key = $1`, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, false, nil
	}
	return order, err == nil, err
}

func readOrder(ctx context.Context, q querier, predicate string, value any) (domain.Order, error) {
	var order domain.Order
	err := q.QueryRow(ctx, `
		SELECT o.id, o.cart_id, o.idempotency_key, o.subtotal_cents, o.discount_cents,
		       o.total_cents, o.created_at, c.code
		FROM orders o
		LEFT JOIN coupons c ON c.redeemed_order_id = o.id
		WHERE `+predicate, value).Scan(
		&order.ID, &order.CartID, &order.IdempotencyKey, &order.SubtotalCents,
		&order.DiscountCents, &order.TotalCents, &order.CreatedAt, &order.CouponCode,
	)
	if err != nil {
		return domain.Order{}, err
	}
	rows, err := q.Query(ctx, `
		SELECT product_id, product_name, quantity, unit_price_cents, line_total_cents
		FROM order_items WHERE order_id = $1 ORDER BY product_id`, order.ID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("get order items: %w", err)
	}
	defer rows.Close()
	order.Items = make([]domain.OrderItem, 0)
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPriceCents, &item.LineTotalCents); err != nil {
			return domain.Order{}, fmt.Errorf("scan order item: %w", err)
		}
		order.Items = append(order.Items, item)
	}
	order.Currency = domain.Currency
	return order, rows.Err()
}

func (s *Store) GenerateCoupon(ctx context.Context) (domain.Coupon, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Coupon{}, fmt.Errorf("begin coupon generation: %w", err)
	}
	defer tx.Rollback(ctx)

	// A transaction-scoped advisory lock serializes the small admin operation
	// across every application instance without locking the orders table.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73920481)`); err != nil {
		return domain.Coupon{}, fmt.Errorf("lock coupon generation: %w", err)
	}
	var orderCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orderCount); err != nil {
		return domain.Coupon{}, fmt.Errorf("count orders: %w", err)
	}
	var milestone int64
	err = tx.QueryRow(ctx, `
		SELECT candidate
		FROM generate_series($1::bigint, $2::bigint, $1::bigint) AS candidate
		WHERE NOT EXISTS (SELECT 1 FROM coupons WHERE milestone_order_count = candidate)
		ORDER BY candidate
		LIMIT 1`, s.couponEveryNOrders, orderCount).Scan(&milestone)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Coupon{}, newError("NO_ELIGIBLE_MILESTONE", "no unrewarded order milestone is currently eligible")
	}
	if err != nil {
		return domain.Coupon{}, fmt.Errorf("find coupon milestone: %w", err)
	}

	coupon := domain.Coupon{
		ID: uuid.New(), MilestoneOrderCount: milestone,
		DiscountPercent: s.couponDiscountPercent, Status: "available",
	}
	// 12 hex characters (48 random bits) keep codes short to type while making
	// collisions and guessing impractical at this scale.
	coupon.Code = fmt.Sprintf("SAVE%d-%s", coupon.DiscountPercent, strings.ToUpper(strings.ReplaceAll(coupon.ID.String(), "-", ""))[:12])
	err = tx.QueryRow(ctx, `
		INSERT INTO coupons (id, code, milestone_order_count, discount_percent)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`, coupon.ID, coupon.Code, coupon.MilestoneOrderCount, coupon.DiscountPercent).Scan(&coupon.CreatedAt)
	if err != nil {
		return domain.Coupon{}, fmt.Errorf("insert coupon: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Coupon{}, fmt.Errorf("commit coupon generation: %w", err)
	}
	return coupon, nil
}

func (s *Store) Report(ctx context.Context) (domain.Report, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Report{}, fmt.Errorf("begin report: %w", err)
	}
	defer tx.Rollback(ctx)

	report := domain.Report{Currency: domain.Currency, PurchasedQuantityByProduct: []domain.ProductQuantity{}}
	rows, err := tx.Query(ctx, `
		SELECT product_id, min(product_name), sum(quantity)
		FROM order_items GROUP BY product_id ORDER BY product_id`)
	if err != nil {
		return domain.Report{}, fmt.Errorf("report product quantities: %w", err)
	}
	for rows.Next() {
		var item domain.ProductQuantity
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity); err != nil {
			rows.Close()
			return domain.Report{}, fmt.Errorf("scan report product: %w", err)
		}
		report.PurchasedQuantityByProduct = append(report.PurchasedQuantityByProduct, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.Report{}, fmt.Errorf("iterate report products: %w", err)
	}
	rows.Close()

	if err := tx.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(subtotal_cents), 0), coalesce(sum(discount_cents), 0), coalesce(sum(total_cents), 0)
		FROM orders`).Scan(&report.TotalOrders, &report.GrossRevenueCents, &report.TotalDiscountsCents, &report.NetRevenueCents); err != nil {
		return domain.Report{}, fmt.Errorf("report order totals: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE redeemed_order_id IS NULL), count(*) FILTER (WHERE redeemed_order_id IS NOT NULL)
		FROM coupons`).Scan(&report.Coupons.Generated, &report.Coupons.Available, &report.Coupons.Redeemed); err != nil {
		return domain.Report{}, fmt.Errorf("report coupons: %w", err)
	}
	report.Coupons.Items, err = listCoupons(ctx, tx)
	if err != nil {
		return domain.Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Report{}, fmt.Errorf("commit report: %w", err)
	}
	return report, nil
}

func listCoupons(ctx context.Context, q querier) ([]domain.Coupon, error) {
	rows, err := q.Query(ctx, `
		SELECT id, code, milestone_order_count, discount_percent, redeemed_order_id, created_at, redeemed_at
		FROM coupons ORDER BY milestone_order_count`)
	if err != nil {
		return nil, fmt.Errorf("list coupons: %w", err)
	}
	defer rows.Close()
	coupons := make([]domain.Coupon, 0)
	for rows.Next() {
		var coupon domain.Coupon
		if err := rows.Scan(&coupon.ID, &coupon.Code, &coupon.MilestoneOrderCount, &coupon.DiscountPercent,
			&coupon.RedeemedOrderID, &coupon.CreatedAt, &coupon.RedeemedAt); err != nil {
			return nil, fmt.Errorf("scan coupon: %w", err)
		}
		coupon.Status = "available"
		if coupon.RedeemedOrderID != nil {
			coupon.Status = "redeemed"
		}
		coupons = append(coupons, coupon)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate coupons: %w", err)
	}
	return coupons, nil
}
