.PHONY: fmt vet lint test sidecar gate

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test -race ./...

sidecar:
	cd sidecar/claude-sdk && npm run typecheck && npm test

gate: fmt vet lint test sidecar
