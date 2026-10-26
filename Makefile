.PHONY: fmt vet lint test gate

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test -race ./...

gate: fmt vet lint test
