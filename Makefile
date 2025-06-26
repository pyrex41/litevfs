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

build: deps $(BUILD_DIR)/distsqlite $(BUILD_DIR)/distsqlite-coordinator $(BUILD_DIR)/distsqlite-inject.so

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Build main sidecar binary
$(BUILD_DIR)/distsqlite: $(BUILD_DIR) $(wildcard cmd/distsqlite/*.go) $(wildcard pkg/**/*.go)
	@echo "Building DistSQLite sidecar..."
	cd cmd/distsqlite && go build $(GO_BUILD_FLAGS) -o ../../$(BUILD_DIR)/distsqlite .

# Build HTTP coordinator binary
$(BUILD_DIR)/distsqlite-coordinator: $(BUILD_DIR) $(wildcard cmd/distsqlite-coordinator/*.go) $(wildcard pkg/**/*.go)
	@echo "Building DistSQLite HTTP coordinator..."
	cd cmd/distsqlite-coordinator && go build $(GO_BUILD_FLAGS) -o ../../$(BUILD_DIR)/distsqlite-coordinator .

# Build LD_PRELOAD injection library
$(BUILD_DIR)/distsqlite-inject.so: $(BUILD_DIR) cmd/distsqlite-inject/inject.c pkg/vfs/vfs.c
	@echo "Building LD_PRELOAD injection library..."
	gcc $(CFLAGS) -o $(BUILD_DIR)/distsqlite-inject.so \
		cmd/distsqlite-inject/inject.c \
		pkg/vfs/vfs.c \
		$(LDFLAGS)

test:
	@echo "Running comprehensive test suite..."
	./test/run_tests.sh

test-unit:
	@echo "Running unit tests only..."
	go test -v ./test/unit ./pkg/rsync

test-integration:
	@echo "Running integration tests only..."
	go test -v ./test/integration

test-benchmarks:
	@echo "Running performance benchmarks..."
	go test -v ./test/benchmarks -bench=. -benchmem

test-vfs:
	@echo "Running VFS unit tests..."
	cd test/unit && gcc -o vfs_test vfs_test.c -lsqlite3 -I../../sqlite && ./vfs_test

test-coverage:
	@echo "Running tests with coverage..."
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

test-race:
	@echo "Running tests with race detection..."
	go test -v -race ./test/unit ./test/integration

test-stress:
	@echo "Running stress tests..."
	go test -v ./test/integration -run TestConcurrent -count=10

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

# HTTP coordination demos
demo-http: build
	@echo "Starting HTTP coordination demo..."
	@echo "Terminal 1: Starting HTTP coordinator"
	$(BUILD_DIR)/distsqlite-coordinator --addr :9090 &
	@sleep 2
	@echo "Terminal 2: Starting sidecar with HTTP coordination"
	$(BUILD_DIR)/distsqlite --coordination http --coordinator-url http://localhost:9090 --data-dir ./demo-data &
	@sleep 2
	@echo "Terminal 3: Running application with DistSQLite"
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so go run simple_demo.go

demo-http-multi: build
	@echo "Starting multi-node HTTP coordination demo..."
	@echo "HTTP coordinator:"
	$(BUILD_DIR)/distsqlite-coordinator --addr :9090 &
	@sleep 2
	@echo "Node 1:"
	$(BUILD_DIR)/distsqlite --coordination http --coordinator-url http://localhost:9090 --hostname node1 --data-dir ./demo-data1 --targets node2:8080 &
	@sleep 2
	@echo "Node 2:"
	$(BUILD_DIR)/distsqlite --coordination http --coordinator-url http://localhost:9090 --hostname node2 --data-dir ./demo-data2 --targets node1:8080 --http-addr :8081 &
	@sleep 2
	@echo "Running demo on node1..."
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so go run simple_demo.go

stop-http-demo:
	@pkill -f distsqlite-coordinator || true
	@pkill -f distsqlite || true
	@echo "HTTP demo stopped"