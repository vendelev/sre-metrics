.PHONY: build test lint up down

up:
	docker compose up -d

down:
	docker compose down

lint:
	cd module && golangci-lint run ./...

lint-fix:
	golines --base-formatter=goimports -m 120 -w ./module
	cd module && golangci-lint run ./... --fix

test:
	go -C module test -cover -race ./...

build-user-login:
	go -C module build -o ../bin/user-login-server ./user-login/cmd/server
