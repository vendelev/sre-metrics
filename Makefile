.PHONY: build test vet lint fmt image-user-auth

build:
	go build -o bin/user-auth ./module/user-auth/cmd/user-auth

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w module

image-user-auth:
	docker build -f module/user-auth/Dockerfile -t metrics-user-auth:local .
