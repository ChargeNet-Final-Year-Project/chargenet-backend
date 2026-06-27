SHELL := /bin/sh

.PHONY: db-up db-down run test test-race vet build fmt cover

db-up:
	docker compose up -d

db-down:
	docker compose down

run:
	go run ./cmd/server

# Packages share a dedicated integration database and must run serially.
test:
	go test -count=1 -p 1 ./...

test-race:
	go test -race -count=1 -p 1 ./...

vet:
	go vet ./...

build:
	go build ./...

fmt:
	gofmt -w ./cmd ./internal ./scripts

# Store behavior is also exercised through API, session and OCPI tests.
cover:
	go test -count=1 -p 1 -coverpkg=./internal/store -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out
