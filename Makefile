.PHONY: build test vet

build:
	go build -o sakutei ./cmd/sakutei

test:
	go test ./...

vet:
	go vet ./...
