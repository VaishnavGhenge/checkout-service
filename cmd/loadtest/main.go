package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type response struct {
	status int
	body   []byte
	header http.Header
}

type measurement struct {
	name      string
	startedAt time.Time
	mu        sync.Mutex
	durations []time.Duration
	statuses  map[int]int
	errors    int
}

func newMeasurement(name string) *measurement {
	return &measurement{name: name, startedAt: time.Now(), statuses: make(map[int]int)}
}

func (m *measurement) record(duration time.Duration, status int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations = append(m.durations, duration)
	if err != nil {
		m.errors++
		return
	}
	m.statuses[status]++
}

func (m *measurement) print() {
	m.mu.Lock()
	defer m.mu.Unlock()
	durations := append([]time.Duration(nil), m.durations...)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	elapsed := time.Since(m.startedAt).Seconds()
	fmt.Printf("%-22s requests=%-7d rps=%-8.1f p50=%-9s p95=%-9s p99=%-9s statuses=%v errors=%d\n",
		m.name, len(durations), float64(len(durations))/elapsed,
		percentile(durations, 0.50), percentile(durations, 0.95), percentile(durations, 0.99),
		m.statuses, m.errors)
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * fraction)
	return values[index].Round(time.Microsecond)
}

type runner struct {
	baseURL string
	client  *http.Client
	pool    *pgxpool.Pool
	seq     atomic.Uint64
}

func main() {
	var baseURL string
	var databaseURL string
	var readDuration time.Duration
	var checkoutDuration time.Duration
	var readWorkers int
	var checkoutWorkers int
	flag.StringVar(&baseURL, "base-url", "http://127.0.0.1:18080", "API base URL")
	flag.StringVar(&databaseURL, "database-url", "", "isolated load-test PostgreSQL URL")
	flag.DurationVar(&readDuration, "read-duration", 10*time.Second, "read scenario duration")
	flag.DurationVar(&checkoutDuration, "checkout-duration", 15*time.Second, "checkout scenario duration")
	flag.IntVar(&readWorkers, "read-workers", 64, "concurrent read workers")
	flag.IntVar(&checkoutWorkers, "checkout-workers", 32, "concurrent checkout workers")
	flag.Parse()

	if databaseURL == "" {
		fatal(errors.New("-database-url is required"))
	}
	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		fatal(fmt.Errorf("parse database URL: %w", err))
	}
	databaseName := strings.TrimPrefix(parsedURL.Path, "/")
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "load") {
		fatal(fmt.Errorf("refusing to reset database %q: its name must contain test or load", databaseName))
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fatal(fmt.Errorf("open database: %w", err))
	}
	defer pool.Close()

	transport := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     30 * time.Second,
	}
	r := &runner{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Transport: transport, Timeout: 10 * time.Second},
		pool:    pool,
	}
	if err := r.waitReady(ctx); err != nil {
		fatal(err)
	}

	fmt.Printf("target=%s read_workers=%d checkout_workers=%d\n", r.baseURL, readWorkers, checkoutWorkers)
	if err := r.runReads(ctx, "empty database", readWorkers, readDuration, true); err != nil {
		fatal(err)
	}
	if err := r.runCheckoutWorkflows(ctx, checkoutWorkers, checkoutDuration); err != nil {
		fatal(err)
	}
	if err := r.runReads(ctx, "populated database", readWorkers, 5*time.Second, false); err != nil {
		fatal(err)
	}
	if err := r.runInventoryContention(ctx); err != nil {
		fatal(err)
	}
	if err := r.runCouponContention(ctx); err != nil {
		fatal(err)
	}
	fmt.Println("all load and invariant checks passed")
}

func (r *runner) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, _, err := r.do(ctx, http.MethodGet, "/health", nil, nil)
		if err == nil && response.status == http.StatusOK {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("API did not become ready within 15 seconds")
}

func (r *runner) reset(ctx context.Context, regularInventory, limitedInventory int) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin load-test reset: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `TRUNCATE coupons, order_items, orders, cart_items, carts, products RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("truncate load-test database: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO products (id, name, price_cents, inventory) VALUES
			(1, 'Mechanical Keyboard', 8999, $1),
			(2, 'Wireless Mouse', 3499, $1),
			(3, 'USB-C Hub', 4999, $1),
			(4, 'Laptop Stand', 5999, $1),
			(5, 'Limited Edition Keycap', 1299, $2)`, regularInventory, limitedInventory); err != nil {
		return fmt.Errorf("seed load-test database: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence('products', 'id'), 5)`); err != nil {
		return fmt.Errorf("advance product sequence: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit load-test reset: %w", err)
	}
	return nil
}

func (r *runner) runReads(ctx context.Context, label string, workers int, duration time.Duration, reset bool) error {
	if reset {
		if err := r.reset(ctx, 1_000_000, 100); err != nil {
			return err
		}
	}
	fmt.Printf("\nread scenario (%s): %s\n", label, duration)
	products := newMeasurement("GET /products")
	report := newMeasurement("GET /admin/report")
	deadline := time.Now().Add(duration)
	var waitGroup sync.WaitGroup
	for worker := range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for time.Now().Before(deadline) {
				metric, path := products, "/products"
				if worker%2 == 1 {
					metric, path = report, "/admin/report"
				}
				response, elapsed, err := r.do(ctx, http.MethodGet, path, nil, nil)
				status := 0
				if response != nil {
					status = response.status
				}
				metric.record(elapsed, status, err)
			}
		}()
	}
	waitGroup.Wait()
	products.print()
	report.print()
	if products.errors != 0 || report.errors != 0 || products.statuses[http.StatusOK] != len(products.durations) || report.statuses[http.StatusOK] != len(report.durations) {
		return errors.New("read scenario returned errors or non-200 responses")
	}
	return nil
}

func (r *runner) runCheckoutWorkflows(ctx context.Context, workers int, duration time.Duration) error {
	if err := r.reset(ctx, 1_000_000, 100); err != nil {
		return err
	}
	fmt.Printf("\ncheckout and exact-retry scenario: %s\n", duration)
	createCart := newMeasurement("POST /carts")
	addItem := newMeasurement("POST /carts/items")
	checkout := newMeasurement("POST /checkout")
	retry := newMeasurement("POST /checkout retry")
	deadline := time.Now().Add(duration)
	var failures atomic.Int64
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for time.Now().Before(deadline) {
				cartID, err := r.createCart(ctx, createCart)
				if err != nil {
					failures.Add(1)
					continue
				}
				if err := r.addItem(ctx, cartID, 1, 1, addItem); err != nil {
					failures.Add(1)
					continue
				}
				key := fmt.Sprintf("load-%d", r.seq.Add(1))
				orderID, err := r.checkout(ctx, cartID, key, "", checkout, false)
				if err != nil {
					failures.Add(1)
					continue
				}
				replayedOrderID, err := r.checkout(ctx, cartID, key, "", retry, true)
				if err != nil || replayedOrderID != orderID {
					failures.Add(1)
				}
			}
		}()
	}
	waitGroup.Wait()
	for _, metric := range []*measurement{createCart, addItem, checkout, retry} {
		metric.print()
	}
	if failures.Load() != 0 {
		return fmt.Errorf("checkout workflow failures=%d", failures.Load())
	}
	return nil
}

func (r *runner) runInventoryContention(ctx context.Context) error {
	const inventory = 100
	const attempts = 200
	if err := r.reset(ctx, 1_000_000, inventory); err != nil {
		return err
	}
	fmt.Printf("\ninventory contention: %d checkouts for %d units\n", attempts, inventory)
	cartIDs := make([]string, attempts)
	for index := range attempts {
		cartID, err := r.createCart(ctx, nil)
		if err != nil {
			return err
		}
		if err := r.addItem(ctx, cartID, 5, 1, nil); err != nil {
			return err
		}
		cartIDs[index] = cartID
	}

	start := make(chan struct{})
	statuses := make(chan int, attempts)
	metric := newMeasurement("contended checkout")
	var waitGroup sync.WaitGroup
	for index, cartID := range cartIDs {
		waitGroup.Add(1)
		go func(index int, cartID string) {
			defer waitGroup.Done()
			<-start
			response, elapsed, err := r.do(ctx, http.MethodPost, "/carts/"+cartID+"/checkout", []byte(`{}`), map[string]string{"Idempotency-Key": fmt.Sprintf("stock-%d", index)})
			status := 0
			if response != nil {
				status = response.status
			}
			metric.record(elapsed, status, err)
			statuses <- status
		}(index, cartID)
	}
	close(start)
	waitGroup.Wait()
	close(statuses)
	metric.print()
	succeeded, conflicted := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			succeeded++
		case http.StatusConflict:
			conflicted++
		}
	}
	var remaining int
	if err := r.pool.QueryRow(ctx, `SELECT inventory FROM products WHERE id = 5`).Scan(&remaining); err != nil {
		return err
	}
	if succeeded != inventory || conflicted != attempts-inventory || remaining != 0 {
		return fmt.Errorf("inventory invariant failed: succeeded=%d conflicted=%d remaining=%d", succeeded, conflicted, remaining)
	}
	return nil
}

func (r *runner) runCouponContention(ctx context.Context) error {
	if err := r.reset(ctx, 1_000_000, 100); err != nil {
		return err
	}
	fmt.Println("\ncoupon generation and redemption contention")
	for index := range 5 {
		cartID, err := r.createCart(ctx, nil)
		if err != nil {
			return err
		}
		if err := r.addItem(ctx, cartID, 1, 1, nil); err != nil {
			return err
		}
		if _, err := r.checkout(ctx, cartID, fmt.Sprintf("coupon-seed-%d", index), "", nil, false); err != nil {
			return err
		}
	}

	const generators = 20
	start := make(chan struct{})
	results := make(chan response, generators)
	metric := newMeasurement("POST /admin/coupons")
	var waitGroup sync.WaitGroup
	for range generators {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			result, elapsed, err := r.do(ctx, http.MethodPost, "/admin/coupons", []byte(`{}`), nil)
			status := 0
			if result != nil {
				status = result.status
				results <- *result
			}
			metric.record(elapsed, status, err)
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)
	metric.print()

	generated, rejected := 0, 0
	var couponCode string
	for result := range results {
		switch result.status {
		case http.StatusCreated:
			generated++
			var coupon struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(result.body, &coupon); err != nil {
				return err
			}
			couponCode = coupon.Code
		case http.StatusConflict:
			rejected++
		}
	}
	if generated != 1 || rejected != generators-1 {
		return fmt.Errorf("coupon generation invariant failed: generated=%d rejected=%d", generated, rejected)
	}

	carts := make([]string, 2)
	for index := range carts {
		cartID, err := r.createCart(ctx, nil)
		if err != nil {
			return err
		}
		if err := r.addItem(ctx, cartID, int64(index+2), 1, nil); err != nil {
			return err
		}
		carts[index] = cartID
	}
	redemptionStart := make(chan struct{})
	redemptionStatuses := make(chan int, len(carts))
	redemptionMetric := newMeasurement("coupon redemption")
	for index, cartID := range carts {
		waitGroup.Add(1)
		go func(index int, cartID string) {
			defer waitGroup.Done()
			<-redemptionStart
			body, _ := json.Marshal(map[string]string{"coupon_code": couponCode})
			result, elapsed, err := r.do(ctx, http.MethodPost, "/carts/"+cartID+"/checkout", body, map[string]string{"Idempotency-Key": fmt.Sprintf("redeem-%d", index)})
			status := 0
			if result != nil {
				status = result.status
			}
			redemptionMetric.record(elapsed, status, err)
			redemptionStatuses <- status
		}(index, cartID)
	}
	close(redemptionStart)
	waitGroup.Wait()
	close(redemptionStatuses)
	redemptionMetric.print()
	redeemed, redemptionConflicts := 0, 0
	for status := range redemptionStatuses {
		if status == http.StatusCreated {
			redeemed++
		} else if status == http.StatusConflict {
			redemptionConflicts++
		}
	}
	if redeemed != 1 || redemptionConflicts != 1 {
		return fmt.Errorf("coupon redemption invariant failed: redeemed=%d conflicted=%d", redeemed, redemptionConflicts)
	}
	report, _, err := r.do(ctx, http.MethodGet, "/admin/report", nil, nil)
	if err != nil {
		return err
	}
	if report.status != http.StatusOK {
		return fmt.Errorf("final report status=%d body=%s", report.status, report.body)
	}
	var summary struct {
		TotalOrders int64 `json:"total_orders"`
		Coupons     struct {
			Generated int64 `json:"generated"`
			Available int64 `json:"available"`
			Redeemed  int64 `json:"redeemed"`
		} `json:"coupons"`
	}
	if err := json.Unmarshal(report.body, &summary); err != nil {
		return err
	}
	if summary.TotalOrders != 6 || summary.Coupons.Generated != 1 || summary.Coupons.Available != 0 || summary.Coupons.Redeemed != 1 {
		return fmt.Errorf("final report does not reconcile: %+v", summary)
	}
	return nil
}

func (r *runner) createCart(ctx context.Context, metric *measurement) (string, error) {
	response, elapsed, err := r.do(ctx, http.MethodPost, "/carts", nil, nil)
	record(metric, elapsed, response, err)
	if err != nil {
		return "", err
	}
	if response.status != http.StatusCreated {
		return "", fmt.Errorf("create cart status=%d body=%s", response.status, response.body)
	}
	var cart struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.body, &cart); err != nil {
		return "", err
	}
	return cart.ID, nil
}

func (r *runner) addItem(ctx context.Context, cartID string, productID int64, quantity int, metric *measurement) error {
	body, _ := json.Marshal(map[string]any{"product_id": productID, "quantity": quantity})
	response, elapsed, err := r.do(ctx, http.MethodPost, "/carts/"+cartID+"/items", body, nil)
	record(metric, elapsed, response, err)
	if err != nil {
		return err
	}
	if response.status != http.StatusCreated {
		return fmt.Errorf("add item status=%d body=%s", response.status, response.body)
	}
	return nil
}

func (r *runner) checkout(ctx context.Context, cartID, key, coupon string, metric *measurement, expectReplay bool) (string, error) {
	body, _ := json.Marshal(map[string]string{"coupon_code": coupon})
	response, elapsed, err := r.do(ctx, http.MethodPost, "/carts/"+cartID+"/checkout", body, map[string]string{"Idempotency-Key": key})
	record(metric, elapsed, response, err)
	if err != nil {
		return "", err
	}
	if response.status != http.StatusCreated {
		return "", fmt.Errorf("checkout status=%d body=%s", response.status, response.body)
	}
	if expectReplay && response.header.Get("Idempotent-Replay") != "true" {
		return "", errors.New("retry response omitted Idempotent-Replay header")
	}
	var order struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.body, &order); err != nil {
		return "", err
	}
	return order.ID, nil
}

func record(metric *measurement, elapsed time.Duration, response *response, err error) {
	if metric == nil {
		return
	}
	status := 0
	if response != nil {
		status = response.status
	}
	metric.record(elapsed, status, err)
}

func (r *runner) do(ctx context.Context, method, path string, body []byte, headers map[string]string) (*response, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	startedAt := time.Now()
	result, err := r.client.Do(request)
	elapsed := time.Since(startedAt)
	if err != nil {
		return nil, elapsed, err
	}
	defer result.Body.Close()
	responseBody, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, elapsed, err
	}
	return &response{status: result.StatusCode, body: responseBody, header: result.Header.Clone()}, elapsed, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "load test failed:", err)
	os.Exit(1)
}
