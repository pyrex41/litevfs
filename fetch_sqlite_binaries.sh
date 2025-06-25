#!/bin/bash

# Detect the operating system
if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS
    SQLITE_URL="https://sqlite.org/2025/sqlite-tools-osx-x64-3500100.zip"
    echo "Detected macOS - downloading SQLite tools for macOS"
elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
    # Linux
    SQLITE_URL="https://sqlite.org/2025/sqlite-tools-linux-x64-3500100.zip"
    echo "Detected Linux - downloading SQLite tools for Linux"
else
    echo "Unsupported operating system: $OSTYPE"
    exit 1
fi

curl -L "$SQLITE_URL" -o sqlite-tools.zip
unzip sqlite-tools.zip
rm -rf sqlite-tools.zip
chmod +x sqlite3_rsync
chmod +x sqlite3
chmod +x sqlite3_analyzer
chmod +x sqldiff




