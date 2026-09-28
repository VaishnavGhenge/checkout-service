.PHONY: run test test-integration verify compose-up compose-down

run:
	go run ./cmd/api

test:
	go test ./...

test-integration:
	TEST_DATABASE_URL="$${TEST_DATABASE_URL:-postgres://checkout:checkout@localhost:5432/checkout?sslmode=disable}" go test -count=1 -tags=integration ./...

verify:
	go fmt ./...
	go vet ./...
	go test ./...

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down

