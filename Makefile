APP_NAME := eth-explorer
BUILD_DIR := bin

.PHONY: build run test lint fmt vet tidy docker-build docker-up docker-down swagger clean

build:
	go build -o $(BUILD_DIR)/$(APP_NAME) ./cmd/server

run: build
	./$(BUILD_DIR)/$(APP_NAME)

test:
	go test ./... -v -cover

lint: vet
	gofmt -l .

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

docker-build:
	docker build -t $(APP_NAME):latest .

docker-up:
	docker compose up --build

docker-down:
	docker compose down

swagger:
	swag init -g cmd/server/main.go -o docs

clean:
	rm -rf $(BUILD_DIR)
