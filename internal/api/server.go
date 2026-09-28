package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vaishnavghenge/checkout-service/docs"
	"github.com/vaishnavghenge/checkout-service/internal/domain"
	"github.com/vaishnavghenge/checkout-service/internal/store"
)

type Service interface {
	Ping(context.Context) error
	Products(context.Context) ([]domain.Product, error)
	CreateCart(context.Context) (domain.Cart, error)
	Cart(context.Context, uuid.UUID) (domain.Cart, error)
	AddCartItem(context.Context, uuid.UUID, int64, int) (domain.Cart, error)
	UpdateCartItem(context.Context, uuid.UUID, int64, int) (domain.Cart, error)
	RemoveCartItem(context.Context, uuid.UUID, int64) error
	Checkout(context.Context, uuid.UUID, string, string) (domain.Order, bool, error)
	Order(context.Context, uuid.UUID) (domain.Order, error)
	GenerateCoupon(context.Context) (domain.Coupon, error)
	Report(context.Context) (domain.Report, error)
}

type Server struct {
	store  Service
	logger *slog.Logger
}

func NewServer(dataStore Service, logger *slog.Logger) http.Handler {
	s := &Server{store: dataStore, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /openapi.yaml", openAPISpec)
	mux.HandleFunc("GET /docs", apiReference)
	mux.HandleFunc("GET /products", s.listProducts)
	mux.HandleFunc("POST /carts", s.createCart)
	mux.HandleFunc("GET /carts/{cartID}", s.getCart)
	mux.HandleFunc("POST /carts/{cartID}/items", s.addCartItem)
	mux.HandleFunc("PUT /carts/{cartID}/items/{productID}", s.updateCartItem)
	mux.HandleFunc("DELETE /carts/{cartID}/items/{productID}", s.removeCartItem)
	mux.HandleFunc("POST /carts/{cartID}/checkout", s.checkout)
	mux.HandleFunc("GET /orders/{orderID}", s.getOrder)
	mux.HandleFunc("POST /admin/coupons", s.generateCoupon)
	mux.HandleFunc("GET /admin/report", s.report)
	return s.recoverPanic(s.logRequests(mux))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func openAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(docs.OpenAPI)
}

// The reference page loads a pinned Scalar build from jsDelivr, so viewing it
// needs internet access; the API itself does not.
const apiReferencePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Checkout API reference</title>
</head>
<body>
<div id="app"></div>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.72.1/dist/browser/standalone.js"></script>
<script>
Scalar.createApiReference('#app', {
  url: '/openapi.yaml',
  darkMode: true,
  agent: { disabled: true },
  mcp: { disabled: true },
  showDeveloperTools: 'never',
})
</script>
</body>
</html>
`

func apiReference(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, apiReferencePage)
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.store.Products(r.Context())
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": products})
}

func (s *Server) createCart(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDecodeError(w, err)
		return
	}
	cart, err := s.store.CreateCart(r.Context())
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	w.Header().Set("Location", "/carts/"+cart.ID.String())
	writeJSON(w, http.StatusCreated, cart)
}

func (s *Server) getCart(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDPath(w, r.PathValue("cartID"), "cart ID")
	if !ok {
		return
	}
	cart, err := s.store.Cart(r.Context(), id)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

type itemRequest struct {
	ProductID int64 `json:"product_id"`
	Quantity  int   `json:"quantity"`
}

func (s *Server) addCartItem(w http.ResponseWriter, r *http.Request) {
	cartID, ok := parseUUIDPath(w, r.PathValue("cartID"), "cart ID")
	if !ok {
		return
	}
	var request itemRequest
	if err := decodeJSON(r, &request, true); err != nil {
		writeDecodeError(w, err)
		return
	}
	if request.ProductID <= 0 || !validQuantity(request.Quantity) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_ITEM", "product_id must be positive and quantity must be between 1 and 1000000")
		return
	}
	cart, err := s.store.AddCartItem(r.Context(), cartID, request.ProductID, request.Quantity)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cart)
}

func (s *Server) updateCartItem(w http.ResponseWriter, r *http.Request) {
	cartID, ok := parseUUIDPath(w, r.PathValue("cartID"), "cart ID")
	if !ok {
		return
	}
	productID, ok := parsePositiveInt64Path(w, r.PathValue("productID"), "product ID")
	if !ok {
		return
	}
	var request struct {
		Quantity int `json:"quantity"`
	}
	if err := decodeJSON(r, &request, true); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !validQuantity(request.Quantity) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_QUANTITY", "quantity must be between 1 and 1000000; use DELETE to remove an item")
		return
	}
	cart, err := s.store.UpdateCartItem(r.Context(), cartID, productID, request.Quantity)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

func (s *Server) removeCartItem(w http.ResponseWriter, r *http.Request) {
	cartID, ok := parseUUIDPath(w, r.PathValue("cartID"), "cart ID")
	if !ok {
		return
	}
	productID, ok := parsePositiveInt64Path(w, r.PathValue("productID"), "product ID")
	if !ok {
		return
	}
	if err := s.store.RemoveCartItem(r.Context(), cartID, productID); err != nil {
		s.respondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	cartID, ok := parseUUIDPath(w, r.PathValue("cartID"), "cart ID")
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key header is required and must contain at most 200 characters")
		return
	}
	var request struct {
		CouponCode string `json:"coupon_code"`
	}
	if err := decodeJSON(r, &request, false); err != nil {
		writeDecodeError(w, err)
		return
	}
	order, replayed, err := s.store.Checkout(r.Context(), cartID, idempotencyKey, request.CouponCode)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/orders/"+order.ID.String())
	writeJSON(w, http.StatusCreated, order)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDPath(w, r.PathValue("orderID"), "order ID")
	if !ok {
		return
	}
	order, err := s.store.Order(r.Context(), id)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) generateCoupon(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := decodeJSON(r, &body, false); err != nil {
		writeDecodeError(w, err)
		return
	}
	coupon, err := s.store.GenerateCoupon(r.Context())
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, coupon)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.Report(r.Context())
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func validQuantity(quantity int) bool { return quantity > 0 && quantity <= 1_000_000 }

func parseUUIDPath(w http.ResponseWriter, value, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_PATH_PARAMETER", name+" must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

func parsePositiveInt64Path(w http.ResponseWriter, value, name string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_PATH_PARAMETER", name+" must be a positive integer")
		return 0, false
	}
	return id, true
}

func decodeJSON(r *http.Request, destination any, required bool) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) && !required {
			return nil
		}
		return fmt.Errorf("request body must contain one valid JSON object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain only one JSON object")
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", fmt.Sprintf("request body must be at most %d bytes", tooLarge.Limit))
		return
	}
	writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	var response errorEnvelope
	response.Error.Code = code
	response.Error.Message = message
	writeJSON(w, status, response)
}

func (s *Server) respondError(w http.ResponseWriter, r *http.Request, err error) {
	var domainErr *store.Error
	if errors.As(err, &domainErr) {
		status := http.StatusConflict
		switch domainErr.Code {
		case "NOT_FOUND", "COUPON_NOT_FOUND":
			status = http.StatusNotFound
		case "EMPTY_CART":
			status = http.StatusUnprocessableEntity
		}
		writeAPIError(w, status, domainErr.Code, domainErr.Message)
		return
	}
	if store.IsRetryable(err) {
		// The transaction rolled back, so nothing changed. Checkout retries are
		// safe with the same Idempotency-Key.
		s.logger.Warn("request hit database contention", "method", r.Method, "path", r.URL.Path, "error", err)
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusServiceUnavailable, "RETRYABLE_CONFLICT", "the request conflicted with concurrent activity and was not applied; retry it")
		return
	}
	s.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic recovered", "method", r.Method, "path", r.URL.Path, "panic", recovered)
				writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
