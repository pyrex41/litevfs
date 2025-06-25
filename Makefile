.PHONY: build clean test deps install

# Build directories
BUILD_DIR := build
PKG_DIR := pkg
CMD_DIR := cmd

# Compiler flags
CFLAGS := -fPIC -shared -O2 -g -Wall
LDFLAGS := -ldl -lpthread -lrt

# Go build flags
GO_BUILD_FLAGS := -v -trimpath

all: build

deps:
	@echo "Installing dependencies..."
	./fetch_sqlite_binaries.sh
	@echo "Checking for SQLite development headers..."
	@if ! pkg-config --exists sqlite3; then \
		echo "SQLite development headers not found. Installing..."; \
		sudo apt-get update && sudo apt-get install -y libsqlite3-dev; \
	fi
	go mod init distsqlite 2>/dev/null || true
	go mod tidy

build: deps $(BUILD_DIR)/distsqlite $(BUILD_DIR)/distsqlite-inject.so

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Build main sidecar binary
$(BUILD_DIR)/distsqlite: $(BUILD_DIR) $(wildcard cmd/distsqlite/*.go) $(wildcard pkg/**/*.go)
	@echo "Building DistSQLite sidecar..."
	cd cmd/distsqlite && go build $(GO_BUILD_FLAGS) -o ../../$(BUILD_DIR)/distsqlite .

# Build LD_PRELOAD injection library
$(BUILD_DIR)/distsqlite-inject.so: $(BUILD_DIR) cmd/distsqlite-inject/inject.c pkg/vfs/vfs.c
	@echo "Building LD_PRELOAD injection library..."
	gcc $(CFLAGS) -o $(BUILD_DIR)/distsqlite-inject.so \
		cmd/distsqlite-inject/inject.c \
		pkg/vfs/vfs.c \
		$(LDFLAGS)

test:
	@echo "Running tests..."
	go test -v ./...
	@echo "Running integration tests..."
	cd test/integration && go test -v .

clean:
	rm -rf $(BUILD_DIR)
	rm -f sqlite3 sqlite3_rsync sqlite3_analyzer sqldiff
	# Clean up shared memory segments
	ipcs -m | grep 0x44495354 | awk '{print $$2}' | xargs -r ipcrm -m

install: build
	sudo cp $(BUILD_DIR)/distsqlite /usr/local/bin/
	sudo cp $(BUILD_DIR)/distsqlite-inject.so /usr/local/lib/

benchmark:
	@echo "Running benchmarks against Turso..."
	cd test/benchmark && go test -bench=. -benchmem

# Development helpers
dev-consul:
	consul agent -dev

dev-test: build
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so go run simple_demo.go

dev-debug: build
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so strace -e write go run simple_demo.go

# IPC debugging
ipc-status:
	@echo "Shared memory segments:"
	@ipcs -m | grep 0x44495354 || echo "No DistSQLite shared memory segments found"

ipc-clean:
	@echo "Cleaning up shared memory segments..."
	@ipcs -m | grep 0x44495354 | awk '{print $$2}' | xargs -r ipcrm -m

lint:
	golangci-lint run ./...
	clang-format -i pkg/vfs/*.c cmd/distsqlite-inject/*.c

# Demo targets
demo-simple: build
	@echo "Starting simple demo..."
	@echo "Terminal 1: Starting sidecar"
	$(BUILD_DIR)/distsqlite --data-dir ./demo-data &
	@sleep 2
	@echo "Terminal 2: Running application with DistSQLite"
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so go run simple_demo.go

demo-multi: build
	@echo "Starting multi-node demo..."
	@echo "Make sure Consul is running: make dev-consul"
	@echo "Node 1:"
	$(BUILD_DIR)/distsqlite --hostname node1 --data-dir ./demo-data1 --targets node2:8080 &
	@sleep 2
	@echo "Node 2:"
	$(BUILD_DIR)/distsqlite --hostname node2 --data-dir ./demo-data2 --targets node1:8080 --http-addr :8081 &
	@sleep 2
	@echo "Running demo on node1..."
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so go run simple_demo.go

stop-demo:
	@pkill -f distsqlite || true
	@echo "Demo stopped"