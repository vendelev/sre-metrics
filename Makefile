.PHONY: build test vet lint fmt image-user-login

build:
	go build -o bin/user-login ./service/user-login/cmd

test:
	go test -cover -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	golines --base-formatter=goimports -m 120 -w .

image-user-login:
	docker build -f service/user-login/Dockerfile -t metrics-user-login:local .

up:
	docker compose up -d
