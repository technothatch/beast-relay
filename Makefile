# beast-relay — convenience entry points.
# Every target is a plain command; `make` is optional, the README's
# Development section lists the same commands to run by hand.

.PHONY: all build test fmt fmt-check vet cover clean

all: fmt-check vet build test

build:
	go build ./...

test:
	go test ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

cover:
	go test -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

clean:
	rm -f coverage.out
