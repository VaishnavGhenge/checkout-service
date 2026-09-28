package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vaishnavghenge/checkout-service/internal/domain"
	"github.com/vaishnavghenge/checkout-service/internal/store"
)

type fakeService struct {
	checkout func(context.Context, uuid.UUID, string, string) (domain.Order, bool, error)
	addItem  func(context.Context, uuid.UUID, int64, int) (domain.Cart, error)
}

func (f *fakeService) Ping(context.Context) error { return nil }
func (f *fakeService) Products(context.Context) ([]domain.Product, error) {
	return []domain.Product{}, nil
}
func (f *fakeService) CreateCart(context.Context) (domain.Cart, error) {
	return domain.Cart{}, nil
}
func (f *fakeService) Cart(context.Context, uuid.UUID) (domain.Cart, error) {
	return domain.Cart{}, nil
}
func (f *fakeService) AddCartItem(ctx context.Context, cartID uuid.UUID, productID int64, quantity int) (domain.Cart, error) {
	if f.addItem != nil {
		return f.addItem(ctx, cartID, productID, quantity)
	}
	return domain.Cart{}, nil
}
func (f *fakeService) UpdateCartItem(context.Context, uuid.UUID, int64, int) (domain.Cart, error) {
	return domain.Cart{}, nil
}
func (f *fakeService) RemoveCartItem(context.Context, uuid.UUID, int64) error { return nil }
func (f *fakeService) Checkout(ctx context.Context, cartID uuid.UUID, key, coupon string) (domain.Order, bool, error) {
	if f.checkout != nil {
		return f.checkout(ctx, cartID, key, coupon)
	}
	return domain.Order{}, false, nil
}
func (f *fakeService) Order(context.Context, uuid.UUID) (domain.Order, error) {
	return domain.Order{}, nil
}
func (f *fakeService) GenerateCoupon(context.Context) (domain.Coupon, error) {
	return domain.Coupon{}, nil
}
func (f *fakeService) Report(context.Context) (domain.Report, error) {
	return domain.Report{}, nil
}

func testServer(service Service) http.Handler {
	return NewServer(service, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCheckoutReplayContract(t *testing.T) {
	cartID := uuid.New()
	orderID := uuid.New()
	service := &fakeService{checkout: func(_ context.Context, gotCartID uuid.UUID, key, coupon string) (domain.Order, bool, error) {
		if gotCartID != cartID || key != "retry-key" || coupon != "SAVE10" {
			t.Fatalf("unexpected checkout input: cart=%s key=%q coupon=%q", gotCartID, key, coupon)
		}
		return domain.Order{ID: orderID, CartID: cartID, Items: []domain.OrderItem{}, Currency: domain.Currency}, true, nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/carts/"+cartID.String()+"/checkout", strings.NewReader(`{"coupon_code":"SAVE10"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key")
	response := httptest.NewRecorder()

	testServer(service).ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("missing Idempotent-Replay header: %v", response.Header())
	}
	if response.Header().Get("Location") != "/orders/"+orderID.String() {
		t.Fatalf("unexpected Location header: %q", response.Header().Get("Location"))
	}
}

func TestCheckoutRequiresIdempotencyKey(t *testing.T) {
	cartID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/carts/"+cartID.String()+"/checkout", strings.NewReader(`{}`))
	response := httptest.NewRecorder()

	testServer(&fakeService{}).ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY")
}

func TestAddItemRejectsUnknownJSONField(t *testing.T) {
	cartID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/carts/"+cartID.String()+"/items", strings.NewReader(`{"product_id":1,"quantity":1,"price":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	testServer(&fakeService{}).ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestDomainErrorsHaveStableHTTPMapping(t *testing.T) {
	tests := []struct {
		code       string
		wantStatus int
	}{
		{code: "NOT_FOUND", wantStatus: http.StatusNotFound},
		{code: "COUPON_NOT_FOUND", wantStatus: http.StatusNotFound},
		{code: "EMPTY_CART", wantStatus: http.StatusUnprocessableEntity},
		{code: "INSUFFICIENT_INVENTORY", wantStatus: http.StatusConflict},
		{code: "COUPON_ALREADY_REDEEMED", wantStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			service := &fakeService{checkout: func(context.Context, uuid.UUID, string, string) (domain.Order, bool, error) {
				return domain.Order{}, false, &store.Error{Code: test.code, Message: "test message"}
			}}
			request := httptest.NewRequest(http.MethodPost, "/carts/"+uuid.NewString()+"/checkout", strings.NewReader(`{}`))
			request.Header.Set("Idempotency-Key", "test-key")
			response := httptest.NewRecorder()
			testServer(service).ServeHTTP(response, request)
			assertAPIError(t, response, test.wantStatus, test.code)
		})
	}
}

func TestDatabaseContentionIsRetryable(t *testing.T) {
	for _, code := range []string{"55P03", "57014", "40P01", "40001"} {
		t.Run(code, func(t *testing.T) {
			service := &fakeService{checkout: func(context.Context, uuid.UUID, string, string) (domain.Order, bool, error) {
				return domain.Order{}, false, fmt.Errorf("lock checkout products: %w", &pgconn.PgError{Code: code})
			}}
			request := httptest.NewRequest(http.MethodPost, "/carts/"+uuid.NewString()+"/checkout", strings.NewReader(`{}`))
			request.Header.Set("Idempotency-Key", "test-key")
			response := httptest.NewRecorder()
			testServer(service).ServeHTTP(response, request)
			assertAPIError(t, response, http.StatusServiceUnavailable, "RETRYABLE_CONFLICT")
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("retryable response has no Retry-After header")
			}
		})
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	body := `{"product_id":1,"quantity":1,"padding":"` + strings.Repeat("x", 1<<20) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/carts/"+uuid.NewString()+"/items", strings.NewReader(body))
	response := httptest.NewRecorder()

	testServer(&fakeService{}).ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE")
}

func TestServesOpenAPIContractAndReference(t *testing.T) {
	handler := testServer(&fakeService{})

	spec := httptest.NewRecorder()
	handler.ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if spec.Code != http.StatusOK || !strings.HasPrefix(spec.Body.String(), "openapi: 3.1.0") {
		t.Fatalf("spec status=%d body starts %.40q", spec.Code, spec.Body.String())
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "url: '/openapi.yaml'") {
		t.Fatalf("docs status=%d body=%s", page.Code, page.Body.String())
	}
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), status)
	}
	var body errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != code {
		t.Fatalf("error code=%q, want %q", body.Error.Code, code)
	}
}
