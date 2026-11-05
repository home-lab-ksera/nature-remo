.PHONY: test test-race run build image
test:
	CGO_ENABLED=0 go test ./...
	CGO_ENABLED=0 go vet ./...
	node --check web/app.js
test-race:
	go test -race ./...
run:
	go run .
build:
	CGO_ENABLED=0 go build -trimpath -o nature-remo .
image:
	docker build -t nature-remo:dev .
