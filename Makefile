.PHONY: build test vet lint fmt image-user-auth

build:
	go build -o bin/user-auth ./service/user-auth/cmd

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w service

image-user-auth:
	docker build -f service/user-auth/Dockerfile -t metrics-user-auth:local .
