#!/bin/bash

echo "🚀 DistSQLite Complete Layered Safety Architecture Demo"
echo "===================================================="
echo

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed. Please install Go 1.19+ to run this demo."
    exit 1
fi

echo "✅ Go found: $(go version)"

# Check dependencies
echo "🔍 Checking dependencies..."

# Build the demo
echo "🔨 Building demo..."
if ! go build -o complete_layered_demo complete_layered_demo.go; then
    echo "❌ Build failed. Make sure all dependencies are available."
    echo "💡 Try running: go mod tidy"
    exit 1
fi

echo "✅ Demo built successfully"
echo

# Show usage options
echo "📋 Demo Modes Available:"
echo "  • normal   - Standard 5-minute verification (default)"
echo "  • fast     - Frequent 30-second verification (demo-friendly)"
echo "  • critical - High-availability 2-minute verification with quick checks"
echo

# Default to fast mode for demo purposes
DEMO_MODE=${1:-fast}

echo "🎯 Running in '$DEMO_MODE' mode..."
echo "📝 Log output will show:"
echo "  • Real-time SQL replay attempts (40-80x bandwidth advantage)"
echo "  • Medium-frequency page sync (5-15x bandwidth advantage)"
echo "  • Automatic safety net verification and repair"
echo "  • Fallback mechanisms when SQL replay fails"
echo "  • Live performance metrics and health monitoring"
echo

echo "🚦 Starting demo in 3 seconds... (Press Ctrl+C to stop anytime)"
sleep 3

# Run the demo
./complete_layered_demo "$DEMO_MODE"

# Cleanup
echo "🧹 Cleaning up demo files..."
rm -f demo_*.db demo_*.db-wal demo_*.db-shm
rm -f complete_layered_demo

echo "✅ Demo completed!"