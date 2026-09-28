#!/bin/sh
set -eu

base_url="${BASE_URL:-http://localhost:8080}"
idempotency_key="reviewer-demo-$(date +%s)-$$"

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

echo "1. Seeded products"
curl -fsS "$base_url/products" | jq .

echo "2. Create a cart"
cart="$(curl -fsS -X POST "$base_url/carts")"
echo "$cart" | jq .
cart_id="$(echo "$cart" | jq -r .id)"

echo "3. Add two keyboards"
curl -fsS -X POST "$base_url/carts/$cart_id/items" \
  -H 'Content-Type: application/json' \
  -d '{"product_id":1,"quantity":2}' | jq .

echo "4. Checkout"
curl -fsS -X POST "$base_url/carts/$cart_id/checkout" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $idempotency_key" \
  -d '{}' | jq .

echo "5. Retry the exact checkout; the order ID remains the same"
curl -fsS -i -X POST "$base_url/carts/$cart_id/checkout" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $idempotency_key" \
  -d '{}'

echo "6. Read-only administration report"
curl -fsS "$base_url/admin/report" | jq .
