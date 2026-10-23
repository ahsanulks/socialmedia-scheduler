GO ?= go

.PHONY: build run test vet fmt tidy clean

build:
	$(GO) build -o bin/scheduler ./cmd/api

run:
	$(GO) run ./cmd/api

test:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
