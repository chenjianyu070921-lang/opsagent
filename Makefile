.PHONY: fmt test run

fmt:
	go fmt ./...

test:
	go test ./...

run:
	go run ./cmd/opsagent
