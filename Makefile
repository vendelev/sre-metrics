.PHONY: build test lint up down

up:
	docker compose up -d

down:
	docker compose down

lint:
	cd backend && golangci-lint run ./...

lint-fix:
	golines --base-formatter=goimports -m 120 -w ./backend
	cd backend && golangci-lint run ./... --fix

test:
	go -C backend test -cover -race ./...

build-user-login:
	go -C backend build -o ../bin/user-login-server ./cmd/user-login-httpserver
