.PHONY: build test vet lint fmt up down

up:
	docker compose up -d

down:
	docker compose down

vet:
	go -C module vet ./...

lint:
	cd module && golangci-lint run ./...

fmt:
	golines --base-formatter=goimports -m 120 -w ./module

test: fmt lint vet
	go -C module test -cover -race ./...

build:
	go -C module build -o ../bin/user-login ./user-login/cmd
