.PHONY: build clean test deps install

# Build directories
BUILD_DIR := build
PKG_DIR := pkg
CMD_DIR := cmd
SRC_DIR := src
BENCH_DIR := bench

# Compiler flags
CFLAGS := -fPIC -shared -O2 -g -Wall -mavx2
LDFLAGS := -ldl -lpthread -lrt

# Go build flags
GO_BUILD_FLAGS := -v -trimpath

# OCaml build flags
OCAML_BUILD_FLAGS := --profile dev

all: build

deps:
	@echo "Installing dependencies..."
	./fetch_sqlite_binaries.sh
	@echo "Checking for SQLite development headers..."
	@if ! pkg-config --exists sqlite3; then \
		echo "SQLite development headers not found. Installing..."; \
		sudo apt-get update && sudo apt-get install -y libsqlite3-dev; \
	fi
	@echo "Checking for OCaml and dune..."
	@if ! command -v ocaml >/dev/null 2>&1; then \
		echo "OCaml not found. Installing..."; \
		sudo apt-get update && sudo apt-get install -y ocaml opam; \
		opam init -y && opam install -y dune bigarray-compat core_bench alcotest; \
	fi
	@if ! command -v dune >/dev/null 2>&1; then \
		echo "Dune not found. Installing..."; \
		opam install -y dune; \
	fi
	go mod init distsqlite 2>/dev/null || true
	go mod tidy

build: deps $(BUILD_DIR)/libdistcore.so $(BUILD_DIR)/distsqlite $(BUILD_DIR)/distsqlite-coordinator $(BUILD_DIR)/distsqlite-inject.so

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Build OCaml performance core
$(BUILD_DIR)/libdistcore.so: $(BUILD_DIR) $(wildcard src/*.ml) $(wildcard src/*.mli) src/distcore_stubs.c
	@echo "Building OCaml performance core..."
	cd $(SRC_DIR) && dune build $(OCAML_BUILD_FLAGS)
	cd $(SRC_DIR) && dune exec -- gcc -shared -fPIC -O3 -mavx2 -DCAML_NAME_SPACE \
		-o ../$(BUILD_DIR)/libdistcore.so \
		distcore_stubs.c \
		_build/default/distcore.cma \
		-I $(shell ocamlfind query -long-format caml) \
		-I $(shell ocaml -where) \
		-lasmrun_shared -lm -lunix -ldl

# Build main sidecar binary
$(BUILD_DIR)/distsqlite: $(BUILD_DIR)/libdistcore.so $(wildcard cmd/distsqlite/*.go) $(wildcard pkg/**/*.go)
	@echo "Building DistSQLite sidecar..."
	cd cmd/distsqlite && CGO_LDFLAGS="-L../../$(BUILD_DIR) -ldistcore" go build $(GO_BUILD_FLAGS) -o ../../$(BUILD_DIR)/distsqlite .

# Build HTTP coordinator binary
$(BUILD_DIR)/distsqlite-coordinator: $(BUILD_DIR)/libdistcore.so $(wildcard cmd/distsqlite-coordinator/*.go) $(wildcard pkg/**/*.go)
	@echo "Building DistSQLite HTTP coordinator..."
	cd cmd/distsqlite-coordinator && CGO_LDFLAGS="-L../../$(BUILD_DIR) -ldistcore" go build $(GO_BUILD_FLAGS) -o ../../$(BUILD_DIR)/distsqlite-coordinator .

# Build LD_PRELOAD injection library
$(BUILD_DIR)/distsqlite-inject.so: $(BUILD_DIR) cmd/distsqlite-inject/inject.c pkg/vfs/vfs.c
	@echo "Building LD_PRELOAD injection library..."
	gcc $(CFLAGS) -o $(BUILD_DIR)/distsqlite-inject.so \
		cmd/distsqlite-inject/inject.c \
		pkg/vfs/vfs.c \
		$(LDFLAGS)

test: test-ocaml test-go

test-ocaml:
	@echo "Running OCaml core tests..."
	cd $(SRC_DIR) && dune runtest

test-go:
	@echo "Running comprehensive Go test suite..."
	./test/run_tests.sh

test-unit:
	@echo "Running unit tests only..."
	go test -v ./test/unit ./pkg/rsync ./pkg/distcore

test-integration:
	@echo "Running integration tests only..."
	go test -v ./test/integration

test-benchmarks:
	@echo "Running performance benchmarks..."
	go test -v ./test/benchmarks -bench=. -benchmem

# OCaml performance benchmarks
test-benchmarks-ocaml: $(BUILD_DIR)/libdistcore.so
	@echo "Running OCaml performance benchmarks..."
	cd $(BENCH_DIR) && dune exec ./bench_distcore.exe

# Comprehensive performance comparison
bench-comparison: test-benchmarks test-benchmarks-ocaml
	@echo "Performance comparison complete!"
	@echo "Check benchmark results above for 10x improvement validation"

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
	# Clean OCaml build artifacts
	cd $(SRC_DIR) && dune clean 2>/dev/null || true
	cd $(BENCH_DIR) && dune clean 2>/dev/null || true
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
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so LD_LIBRARY_PATH=../../$(BUILD_DIR) go run simple_demo.go

dev-debug: build
	cd test/integration && LD_PRELOAD=../../$(BUILD_DIR)/distsqlite-inject.so LD_LIBRARY_PATH=../../$(BUILD_DIR) strace -e write go run simple_demo.go

# OCaml development targets
dev-ocaml-repl:
	cd $(SRC_DIR) && dune utop

dev-ocaml-watch:
	cd $(SRC_DIR) && dune build --watch

dev-perf-test: $(BUILD_DIR)/libdistcore.so
	@echo "Running performance validation..."
	cd $(BENCH_DIR) && dune exec ./bench_distcore.exe | grep -E "(PASS|WARN|BENCH)"

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