# Makefile for edfx-backup

BINARY_NAME=edfx-backup
DIST_DIR=./dist

all: build test

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(DIST_DIR)
	@go build -o $(DIST_DIR)/$(BINARY_NAME) ./cmd/edfx-backup/main.go
	@chmod +x $(DIST_DIR)/$(BINARY_NAME)
	@echo "Build completed: $(DIST_DIR)/$(BINARY_NAME)"

install: build
	@echo "Installing $(BINARY_NAME) to ~/.local/bin/$(BINARY_NAME)..."
	@mkdir -p $(HOME)/.local/bin
	@cp $(DIST_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)
	@echo "Installed to $(HOME)/.local/bin/$(BINARY_NAME)"

run:
	@go run ./cmd/edfx-backup/main.go

test:
	@echo "Running unit tests..."
	@go test ./... -v

clean:
	@echo "Cleaning..."
	@rm -rf $(DIST_DIR) $(BINARY_NAME)

.PHONY: all build install run test clean
