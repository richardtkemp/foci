.PHONY: build test lint deploy clean

APP_NAME := widget-api
VERSION := 2.4.1
GO_FLAGS := -ldflags "-X main.version=$(VERSION)"

build:
	go build $(GO_FLAGS) -o bin/$(APP_NAME) ./cmd/server

test:
	go test -v -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

deploy-staging:
	./scripts/deploy.sh staging

deploy-production:
	@echo "Production deploy requires approval"
	@read -p "Confirm (y/N): " confirm && [ "$$confirm" = "y" ]
	./scripts/deploy.sh production

clean:
	rm -rf bin/ coverage.out

docker-up:
	docker compose up -d

docker-down:
	docker compose down

migrate:
	go run ./cmd/migrate -direction up

migrate-down:
	go run ./cmd/migrate -direction down -steps 1
