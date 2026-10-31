.PHONY: run build test race bench vet fmt tidy \
	docker-build docker-run compose-up compose-down compose-logs \
	test-db-up test-db-down test-integration migrate

build:
	go build -o teahub-server ./backend/cmd/server

run: build
	./teahub-server

# Regenerates go.sum from go.mod. Required once after cloning (network
# access needed) before build/test/vet will work — see README "First-time
# setup".
tidy:
	go mod tidy

test:
	go test ./... -v

race:
	go test -race ./...

bench:
	go test ./backend/internal/index/... -run '^$$' -bench . -benchmem

vet:
	go vet ./...

fmt:
	gofmt -l .

fmt-write:
	gofmt -w .

# --- Docker / local stack -------------------------------------------------

docker-build:
	docker build -t teahub:local .

docker-run: docker-build
	docker run --rm -p 8080:8080 --env-file .env teahub:local

compose-up:
	docker compose up --build

compose-down:
	docker compose down -v

compose-logs:
	docker compose logs -f app

# --- DB integration tests -------------------------------------------------
# Spins up a throwaway Postgres via the docker-compose "test" profile, runs
# dbstore's integration tests against it, then tears it down. See
# backend/internal/dbstore/dbstore_integration_test.go.

test-db-up:
	docker compose --profile test up -d db-test
	@echo "waiting for db-test to be healthy..."
	@until docker compose exec -T db-test pg_isready -U teahub_test -d teahub_test >/dev/null 2>&1; do sleep 1; done

test-db-down:
	docker compose --profile test down -v

test-integration: test-db-up
	TEAHUB_TEST_DATABASE_URL="postgres://teahub_test:teahub_test@localhost:5433/teahub_test?sslmode=disable" \
		go test ./backend/internal/dbstore/... -v -count=1
	$(MAKE) test-db-down
