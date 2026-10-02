.PHONY: build test clean helper server helper-gui

BIN ?= bin

build: helper server helper-gui test

helper:
	mkdir -p $(BIN)
	go build -trimpath -ldflags="-s -w" -o $(BIN)/helper ./cmd/helper

server:
	mkdir -p $(BIN)
	go build -trimpath -ldflags="-s -w" -o $(BIN)/server ./cmd/server

helper-gui:
	mkdir -p $(BIN)
	go build -trimpath -ldflags="-s -w" -o $(BIN)/helper-gui ./cmd/helper-gui

test:
	go test ./...

clean:
	rm -rf $(BIN)
