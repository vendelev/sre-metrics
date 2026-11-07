.PHONY: build test lint up down

up:
	docker compose up -d

down:
	docker compose down

lint:
	golines --base-formatter=goimports -m 120 -w ./module
	cd module && golangci-lint run ./... --fix

test:
	cd module && golangci-lint run ./...
	go -C module test -cover -race ./...

build:
	go -C module build -o ../bin/user-login ./user-login/cmd
