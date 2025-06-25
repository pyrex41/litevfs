# DistSQLite Production Build System
# =================================

# Project configuration
PROJECT_NAME := distsqlite
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")

# Go configuration
GO := go
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
CGO_ENABLED := 1

# C compiler configuration
CC := gcc
CFLAGS := -O2 -Wall -Wextra -fPIC -pthread
LDFLAGS := -shared -lpthread -ldl -lrt

# Build directories
BUILD_DIR := build
BIN_DIR := $(BUILD_DIR)/bin
LIB_DIR := $(BUILD_DIR)/lib
DIST_DIR := $(BUILD_DIR)/dist

# Source files
VFS_C_SRC := pkg/vfs/vfs.c
VFS_LIB := $(LIB_DIR)/libdistsqlite_vfs.so
MAIN_GO_SRC := cmd/distsqlite/main.go
INJECT_C_SRC := cmd/distsqlite-inject/inject.c

# Binary outputs
DISTSQLITE_BIN := $(BIN_DIR)/distsqlite
INJECT_BIN := $(BIN_DIR)/distsqlite-inject

# Test configuration
TEST_DIRS := ./pkg/... ./cmd/...
INTEGRATION_TEST_DIR := ./test/integration
COVERAGE_OUT := $(BUILD_DIR)/coverage.out
COVERAGE_HTML := $(BUILD_DIR)/coverage.html

# Production configuration
DOCKER_REGISTRY := ""
DOCKER_TAG := $(VERSION)
HELM_CHART_DIR := deployments/helm

# Colors for pretty output
RED := \033[31m
GREEN := \033[32m
YELLOW := \033[33m
BLUE := \033[34m
RESET := \033[0m

.PHONY: all clean build test integration-test coverage docker helm lint security check deps help

# Default target
all: clean deps build test ## Build everything and run tests

help: ## Show this help message
	@echo "$(BLUE)DistSQLite Production Build System$(RESET)"
	@echo "=================================="
	@echo ""
	@echo "$(YELLOW)Available targets:$(RESET)"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  $(GREEN)%-20s$(RESET) %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo ""
	@echo "$(YELLOW)Build information:$(RESET)"
	@echo "  Version:     $(VERSION)"
	@echo "  Build time:  $(BUILD_TIME)"
	@echo "  Git commit:  $(GIT_COMMIT)"
	@echo "  Go OS/Arch:  $(GOOS)/$(GOARCH)"

# Dependency management
deps: ## Download and verify Go dependencies
	@echo "$(BLUE)Downloading Go dependencies...$(RESET)"
	$(GO) mod download
	$(GO) mod verify
	$(GO) mod tidy

# Create build directories
$(BUILD_DIR):
	@mkdir -p $(BUILD_DIR) $(BIN_DIR) $(LIB_DIR) $(DIST_DIR)

# Build VFS C library
$(VFS_LIB): $(VFS_C_SRC) | $(BUILD_DIR)
	@echo "$(BLUE)Building VFS C library...$(RESET)"
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $<
	@echo "$(GREEN)✓ VFS library built: $@$(RESET)"

# Build injection library
$(INJECT_BIN): $(INJECT_C_SRC) | $(BUILD_DIR)
	@echo "$(BLUE)Building injection library...$(RESET)"
	$(CC) $(CFLAGS) -o $@ $< -ldl
	@echo "$(GREEN)✓ Injection library built: $@$(RESET)"

# Build main Go binary
$(DISTSQLITE_BIN): $(shell find . -name "*.go" -not -path "./test/*") $(VFS_LIB) | $(BUILD_DIR)
	@echo "$(BLUE)Building DistSQLite main binary...$(RESET)"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build \
		-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)" \
		-o $@ ./cmd/distsqlite
	@echo "$(GREEN)✓ Main binary built: $@$(RESET)"

# Build everything
build: $(VFS_LIB) $(INJECT_BIN) $(DISTSQLITE_BIN) ## Build all binaries and libraries
	@echo "$(GREEN)✓ All components built successfully$(RESET)"

# Download SQLite binaries if needed
sqlite-binaries: ## Download SQLite binaries
	@if [ ! -f sqlite3 ] || [ ! -f sqlite3_rsync ]; then \
		echo "$(BLUE)Downloading SQLite binaries...$(RESET)"; \
		chmod +x fetch_sqlite_binaries.sh; \
		./fetch_sqlite_binaries.sh; \
		echo "$(GREEN)✓ SQLite binaries downloaded$(RESET)"; \
	else \
		echo "$(GREEN)✓ SQLite binaries already present$(RESET)"; \
	fi

# Run unit tests
test: build ## Run unit tests
	@echo "$(BLUE)Running unit tests...$(RESET)"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) test -v -race -timeout=30s $(TEST_DIRS)
	@echo "$(GREEN)✓ Unit tests passed$(RESET)"

# Run integration tests
integration-test: build sqlite-binaries ## Run integration tests
	@echo "$(BLUE)Running integration tests...$(RESET)"
	cd $(INTEGRATION_TEST_DIR) && CGO_ENABLED=$(CGO_ENABLED) $(GO) test -v -timeout=5m ./...
	@echo "$(GREEN)✓ Integration tests passed$(RESET)"

# Generate test coverage
coverage: build ## Generate test coverage report
	@echo "$(BLUE)Generating test coverage...$(RESET)"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) test -coverprofile=$(COVERAGE_OUT) -covermode=atomic $(TEST_DIRS)
	$(GO) tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	$(GO) tool cover -func=$(COVERAGE_OUT)
	@echo "$(GREEN)✓ Coverage report generated: $(COVERAGE_HTML)$(RESET)"

# Lint code
lint: ## Run code linters
	@echo "$(BLUE)Running linters...$(RESET)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
		echo "$(GREEN)✓ Go linting passed$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ golangci-lint not installed, skipping Go linting$(RESET)"; \
	fi
	@if command -v cppcheck >/dev/null 2>&1; then \
		cppcheck --enable=all --error-exitcode=1 $(VFS_C_SRC) $(INJECT_C_SRC); \
		echo "$(GREEN)✓ C/C++ linting passed$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ cppcheck not installed, skipping C linting$(RESET)"; \
	fi

# Security checks
security: ## Run security checks
	@echo "$(BLUE)Running security checks...$(RESET)"
	@if command -v gosec >/dev/null 2>&1; then \
		gosec -fmt json -out $(BUILD_DIR)/gosec-report.json -severity medium ./...; \
		echo "$(GREEN)✓ Go security scan completed$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ gosec not installed, skipping Go security scan$(RESET)"; \
	fi
	@if command -v nancy >/dev/null 2>&1; then \
		$(GO) list -json -deps ./... | nancy sleuth; \
		echo "$(GREEN)✓ Dependency vulnerability scan completed$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ nancy not installed, skipping dependency scan$(RESET)"; \
	fi

# Memory leak detection
memcheck: build ## Run memory leak detection
	@echo "$(BLUE)Running memory leak detection...$(RESET)"
	@if command -v valgrind >/dev/null 2>&1; then \
		valgrind --tool=memcheck --leak-check=full --show-leak-kinds=all \
			--track-origins=yes --verbose --log-file=$(BUILD_DIR)/valgrind.log \
			$(DISTSQLITE_BIN) --help; \
		echo "$(GREEN)✓ Memory check completed, see $(BUILD_DIR)/valgrind.log$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ valgrind not installed, skipping memory check$(RESET)"; \
	fi

# Comprehensive check (all quality checks)
check: deps lint security test integration-test coverage ## Run all quality checks
	@echo "$(GREEN)✓ All quality checks passed$(RESET)"

# Create distribution package
dist: build ## Create distribution package
	@echo "$(BLUE)Creating distribution package...$(RESET)"
	@rm -rf $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)
	@mkdir -p $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)
	
	# Copy binaries
	@cp $(DISTSQLITE_BIN) $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/
	@cp $(INJECT_BIN) $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/
	@cp $(VFS_LIB) $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/
	
	# Copy SQLite binaries if present
	@if [ -f sqlite3 ]; then cp sqlite3 $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/; fi
	@if [ -f sqlite3_rsync ]; then cp sqlite3_rsync $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/; fi
	@if [ -f sqlite3_analyzer ]; then cp sqlite3_analyzer $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/; fi
	
	# Copy documentation
	@cp README.md $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/
	@cp IMPLEMENTATION_SUMMARY.md $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/
	@if [ -f LICENSE ]; then cp LICENSE $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION)/; fi
	
	# Create archive
	@cd $(DIST_DIR) && tar -czf $(PROJECT_NAME)-$(VERSION).tar.gz $(PROJECT_NAME)-$(VERSION)
	@echo "$(GREEN)✓ Distribution package created: $(DIST_DIR)/$(PROJECT_NAME)-$(VERSION).tar.gz$(RESET)"

# Docker build
docker: ## Build Docker image
	@echo "$(BLUE)Building Docker image...$(RESET)"
	docker build -t $(PROJECT_NAME):$(DOCKER_TAG) .
	@if [ -n "$(DOCKER_REGISTRY)" ]; then \
		docker tag $(PROJECT_NAME):$(DOCKER_TAG) $(DOCKER_REGISTRY)/$(PROJECT_NAME):$(DOCKER_TAG); \
		echo "$(GREEN)✓ Docker image tagged for registry: $(DOCKER_REGISTRY)/$(PROJECT_NAME):$(DOCKER_TAG)$(RESET)"; \
	fi
	@echo "$(GREEN)✓ Docker image built: $(PROJECT_NAME):$(DOCKER_TAG)$(RESET)"

# Docker push
docker-push: docker ## Build and push Docker image
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "$(RED)✗ DOCKER_REGISTRY not set$(RESET)"; \
		exit 1; \
	fi
	@echo "$(BLUE)Pushing Docker image...$(RESET)"
	docker push $(DOCKER_REGISTRY)/$(PROJECT_NAME):$(DOCKER_TAG)
	@echo "$(GREEN)✓ Docker image pushed: $(DOCKER_REGISTRY)/$(PROJECT_NAME):$(DOCKER_TAG)$(RESET)"

# Helm chart package
helm: ## Package Helm chart
	@echo "$(BLUE)Packaging Helm chart...$(RESET)"
	@if [ ! -d "$(HELM_CHART_DIR)" ]; then \
		echo "$(YELLOW)⚠ Helm chart directory not found: $(HELM_CHART_DIR)$(RESET)"; \
		exit 0; \
	fi
	@if command -v helm >/dev/null 2>&1; then \
		helm package $(HELM_CHART_DIR) --destination $(DIST_DIR) --version $(VERSION); \
		echo "$(GREEN)✓ Helm chart packaged$(RESET)"; \
	else \
		echo "$(YELLOW)⚠ helm not installed, skipping chart packaging$(RESET)"; \
	fi

# Development server (for testing)
dev: build ## Run development server
	@echo "$(BLUE)Starting development server...$(RESET)"
	$(DISTSQLITE_BIN) \
		--log-level=debug \
		--http-addr=:8080 \
		--data-dir=./dev-data \
		--consul-url=http://localhost:8500 \
		--enable-metrics=true

# Install development tools
install-tools: ## Install development tools
	@echo "$(BLUE)Installing development tools...$(RESET)"
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	$(GO) install github.com/securecodewarrior/go-nancy@latest
	$(GO) install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest
	@echo "$(GREEN)✓ Development tools installed$(RESET)"

# Clean build artifacts
clean: ## Clean build artifacts
	@echo "$(BLUE)Cleaning build artifacts...$(RESET)"
	@rm -rf $(BUILD_DIR)
	@rm -f *.db *.db-shm *.db-wal
	@echo "$(GREEN)✓ Build artifacts cleaned$(RESET)"

# Clean everything including dependencies
clean-all: clean ## Clean everything including downloaded dependencies
	@echo "$(BLUE)Cleaning everything...$(RESET)"
	@rm -f sqlite3 sqlite3_rsync sqlite3_analyzer sqldiff
	$(GO) clean -modcache
	@echo "$(GREEN)✓ Everything cleaned$(RESET)"

# Show build info
info: ## Show build information
	@echo "$(BLUE)Build Information$(RESET)"
	@echo "================="
	@echo "Project:     $(PROJECT_NAME)"
	@echo "Version:     $(VERSION)"
	@echo "Build time:  $(BUILD_TIME)"
	@echo "Git commit:  $(GIT_COMMIT)"
	@echo "Go version:  $(shell $(GO) version)"
	@echo "Go OS/Arch:  $(GOOS)/$(GOARCH)"
	@echo "CGO enabled: $(CGO_ENABLED)"
	@echo "CC:          $(CC)"
	@echo "CFLAGS:      $(CFLAGS)"

# Production deployment check
deploy-check: ## Check production deployment readiness
	@echo "$(BLUE)Checking production deployment readiness...$(RESET)"
	@echo "✓ Checking build status..."
	@$(MAKE) build >/dev/null 2>&1 && echo "  $(GREEN)✓ Build successful$(RESET)" || echo "  $(RED)✗ Build failed$(RESET)"
	@echo "✓ Checking tests..."
	@$(MAKE) test >/dev/null 2>&1 && echo "  $(GREEN)✓ Tests passed$(RESET)" || echo "  $(RED)✗ Tests failed$(RESET)"
	@echo "✓ Checking binaries..."
	@[ -f "$(DISTSQLITE_BIN)" ] && echo "  $(GREEN)✓ Main binary exists$(RESET)" || echo "  $(RED)✗ Main binary missing$(RESET)"
	@[ -f "$(VFS_LIB)" ] && echo "  $(GREEN)✓ VFS library exists$(RESET)" || echo "  $(RED)✗ VFS library missing$(RESET)"
	@echo "✓ Checking SQLite binaries..."
	@[ -f "sqlite3" ] && echo "  $(GREEN)✓ sqlite3 exists$(RESET)" || echo "  $(YELLOW)⚠ sqlite3 missing$(RESET)"
	@[ -f "sqlite3_rsync" ] && echo "  $(GREEN)✓ sqlite3_rsync exists$(RESET)" || echo "  $(YELLOW)⚠ sqlite3_rsync missing$(RESET)"
	@echo "$(GREEN)✓ Deployment readiness check completed$(RESET)"

# Quick development build (skip tests)
quick: deps $(VFS_LIB) $(DISTSQLITE_BIN) ## Quick build for development (skip tests)
	@echo "$(GREEN)✓ Quick build completed$(RESET)"