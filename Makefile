.PHONY: run test test-integration loadtest verify reviewer-check demo compose-up compose-down

run:
	go run ./cmd/api

test:
	go test ./...

test-integration:
	docker compose --profile test up -d --wait test-db
	TEST_DATABASE_URL="$${TEST_DATABASE_URL:-postgres://checkout:checkout@localhost:5433/checkout_test?sslmode=disable}" go test -count=1 -tags=integration ./...

loadtest:
	@test -n "$(LOADTEST_DATABASE_URL)" || (echo "LOADTEST_DATABASE_URL is required" >&2; exit 1)
	go run ./cmd/loadtest -database-url "$(LOADTEST_DATABASE_URL)" $(LOADTEST_ARGS)

verify:
	go fmt ./...
	go vet ./...
	go test ./...

reviewer-check: verify test-integration

demo:
	./scripts/demo.sh

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down
