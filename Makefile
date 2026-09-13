.PHONY: build test clean helper server

BIN ?= bin

build: helper server test

helper:
	mkdir -p $(BIN)
	go build -trimpath -ldflags="-s -w" -o $(BIN)/helper ./cmd/helper

server:
	mkdir -p $(BIN)
	go build -trimpath -ldflags="-s -w" -o $(BIN)/server ./cmd/server

test:
	go test ./...

clean:
	rm -rf $(BIN)
