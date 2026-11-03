.PHONY: build test vet lint fmt image-user-auth

build:
	go build -o bin/user-auth ./service/user-auth/cmd

test:
	go test -cover -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	golines --base-formatter=goimports -m 120 -w .

image-user-auth:
	docker build -f service/user-auth/Dockerfile -t metrics-user-auth:local .
