.PHONY: build test vet lint fmt up

build:
	go -C module build -o ../bin/user-login ./user-login/cmd

test:
	go -C module test -cover -race ./...

vet:
	go -C module vet ./...

lint:
	cd module && golangci-lint run ./...

fmt:
	golines --base-formatter=goimports -m 120 -w ./module

up:
	docker compose up -d
