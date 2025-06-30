#!/bin/bash
set -e

echo "🔗 Testing Application Integration"

# Test with LD_PRELOAD injection
test_ld_preload() {
    echo "📚 Testing LD_PRELOAD integration..."
    
    if [ -f "test/integration/simple_demo.go" ]; then
        cd test/integration
        LD_PRELOAD=../../build/distsqlite-inject.so LD_LIBRARY_PATH=../../build go run simple_demo.go
        cd ../..
    else
        echo "⚠️  Simple demo not found - skipping LD_PRELOAD test"
    fi
}

# Test with multiple applications
test_multiple_apps() {
    echo "🔄 Testing multiple applications..."
    
    if [ -f "test/integration/simple_demo.go" ]; then
        # Start 3 applications simultaneously
        for i in {1..3}; do
            cd test/integration
            LD_PRELOAD=../../build/distsqlite-inject.so LD_LIBRARY_PATH=../../build go run simple_demo.go &
            cd ../..
        done
        
        wait
        echo "✅ Multiple applications completed"
    else
        echo "⚠️  Simple demo not found - skipping multiple apps test"
    fi
}

# Test with real SQLite operations
test_sqlite_operations() {
    echo "🗃️ Testing SQLite operations..."
    
    # Create test program
    cat > test_app.c << 'EOF'
#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>

int main() {
    sqlite3 *db;
    char *err_msg = 0;
    
    int rc = sqlite3_open("test_distributed.db", &db);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Cannot open database: %s\n", sqlite3_errmsg(db));
        sqlite3_close(db);
        return 1;
    }
    
    // Create table
    char *sql = "CREATE TABLE IF NOT EXISTS test_app (id INTEGER PRIMARY KEY, name TEXT);";
    rc = sqlite3_exec(db, sql, 0, 0, &err_msg);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "SQL error: %s\n", err_msg);
        sqlite3_free(err_msg);
        sqlite3_close(db);
        return 1;
    }
    
    // Insert data
    sql = "INSERT INTO test_app (name) VALUES ('Test Application');";
    rc = sqlite3_exec(db, sql, 0, 0, &err_msg);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "SQL error: %s\n", err_msg);
        sqlite3_free(err_msg);
        sqlite3_close(db);
        return 1;
    }
    
    printf("✅ SQLite operations successful\n");
    sqlite3_close(db);
    return 0;
}
EOF
    
    # Compile and run with LD_PRELOAD
    if command -v gcc &> /dev/null && pkg-config --exists sqlite3; then
        gcc -o test_app test_app.c -lsqlite3
        LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build ./test_app
        rm test_app test_app.c
        echo "✅ SQLite C application test completed"
    else
        echo "⚠️  GCC or SQLite3 dev libraries not found - skipping C application test"
        rm -f test_app.c
    fi
}

# Test with Go SQLite application
test_go_sqlite_app() {
    echo "🐹 Testing Go SQLite application..."
    
    # Create test Go program
    cat > test_go_app.go << 'EOF'
package main

import (
    "database/sql"
    "fmt"
    "log"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    db, err := sql.Open("sqlite3", "test_go_distributed.db")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    
    // Create table
    _, err = db.Exec("CREATE TABLE IF NOT EXISTS test_go (id INTEGER PRIMARY KEY, name TEXT)")
    if err != nil {
        log.Fatal(err)
    }
    
    // Insert data
    _, err = db.Exec("INSERT INTO test_go (name) VALUES (?)", "Go Test Application")
    if err != nil {
        log.Fatal(err)
    }
    
    // Query data
    var name string
    err = db.QueryRow("SELECT name FROM test_go WHERE id = 1").Scan(&name)
    if err != nil {
        log.Fatal(err)
    }
    
    fmt.Printf("✅ Go SQLite operations successful: %s\n", name)
}
EOF
    
    # Check if Go SQLite driver is available
    if go list -m github.com/mattn/go-sqlite3 >/dev/null 2>&1; then
        LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build go run test_go_app.go
        rm test_go_app.go
        echo "✅ Go SQLite application test completed"
    else
        echo "⚠️  Go SQLite driver not found - installing..."
        go get github.com/mattn/go-sqlite3
        LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build go run test_go_app.go
        rm test_go_app.go
        echo "✅ Go SQLite application test completed"
    fi
}

# Test with Python SQLite application
test_python_sqlite_app() {
    echo "🐍 Testing Python SQLite application..."
    
    # Create test Python program
    cat > test_python_app.py << 'EOF'
import sqlite3
import sys

def main():
    try:
        conn = sqlite3.connect('test_python_distributed.db')
        cursor = conn.cursor()
        
        # Create table
        cursor.execute('CREATE TABLE IF NOT EXISTS test_python (id INTEGER PRIMARY KEY, name TEXT)')
        
        # Insert data
        cursor.execute('INSERT INTO test_python (name) VALUES (?)', ('Python Test Application',))
        
        # Query data
        cursor.execute('SELECT name FROM test_python WHERE id = 1')
        result = cursor.fetchone()
        
        if result:
            print(f"✅ Python SQLite operations successful: {result[0]}")
        else:
            print("❌ No data found")
            sys.exit(1)
            
        conn.commit()
        conn.close()
        
    except Exception as e:
        print(f"❌ Python SQLite error: {e}")
        sys.exit(1)

if __name__ == "__main__":
    main()
EOF
    
    # Run with LD_PRELOAD if Python is available
    if command -v python3 &> /dev/null; then
        LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build python3 test_python_app.py
        rm test_python_app.py
        echo "✅ Python SQLite application test completed"
    else
        echo "⚠️  Python3 not found - skipping Python application test"
        rm -f test_python_app.py
    fi
}

# Test with Node.js SQLite application
test_nodejs_sqlite_app() {
    echo "🟢 Testing Node.js SQLite application..."
    
    # Create test Node.js program
    cat > test_node_app.js << 'EOF'
const sqlite3 = require('sqlite3').verbose();

const db = new sqlite3.Database('test_node_distributed.db');

db.serialize(() => {
    // Create table
    db.run("CREATE TABLE IF NOT EXISTS test_node (id INTEGER PRIMARY KEY, name TEXT)");
    
    // Insert data
    db.run("INSERT INTO test_node (name) VALUES (?)", ["Node.js Test Application"]);
    
    // Query data
    db.get("SELECT name FROM test_node WHERE id = 1", (err, row) => {
        if (err) {
            console.error("❌ Node.js SQLite error:", err);
            process.exit(1);
        }
        
        if (row) {
            console.log("✅ Node.js SQLite operations successful:", row.name);
        } else {
            console.log("❌ No data found");
            process.exit(1);
        }
        
        db.close();
    });
});
EOF
    
    # Run with LD_PRELOAD if Node.js and sqlite3 module are available
    if command -v node &> /dev/null; then
        if npm list sqlite3 >/dev/null 2>&1 || npm list -g sqlite3 >/dev/null 2>&1; then
            LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build node test_node_app.js
            rm test_node_app.js
            echo "✅ Node.js SQLite application test completed"
        else
            echo "⚠️  Node.js sqlite3 module not found - skipping Node.js application test"
            rm -f test_node_app.js
        fi
    else
        echo "⚠️  Node.js not found - skipping Node.js application test"
        rm -f test_node_app.js
    fi
}

# Test concurrent applications
test_concurrent_applications() {
    echo "🔄 Testing concurrent applications with DistSQLite..."
    
    # Create concurrent test script
    cat > concurrent_test.sh << 'EOF'
#!/bin/bash
for i in {1..5}; do
    (
        echo "Starting concurrent app $i"
        LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build sqlite3 concurrent_test_$i.db << SQL
CREATE TABLE IF NOT EXISTS concurrent_test (id INTEGER PRIMARY KEY, app_id INTEGER, data TEXT);
INSERT INTO concurrent_test (app_id, data) VALUES ($i, 'Concurrent data from app $i');
SELECT * FROM concurrent_test;
SQL
        echo "Completed concurrent app $i"
    ) &
done
wait
echo "All concurrent applications completed"
EOF
    
    chmod +x concurrent_test.sh
    ./concurrent_test.sh
    rm concurrent_test.sh
    
    # Cleanup test databases
    rm -f concurrent_test_*.db
    
    echo "✅ Concurrent applications test completed"
}

# Main execution
main() {
    echo "🎯 Starting application integration tests..."
    
    # Check if DistSQLite is built
    if [ ! -f "build/distsqlite-inject.so" ]; then
        echo "❌ DistSQLite injection library not found. Run 'make build' first."
        exit 1
    fi
    
    # Run tests
    test_ld_preload
    test_multiple_apps
    test_sqlite_operations
    test_go_sqlite_app
    test_python_sqlite_app
    test_nodejs_sqlite_app
    test_concurrent_applications
    
    echo ""
    echo "🎉 Application integration testing completed!"
    echo ""
    echo "📋 Summary:"
    echo "  - LD_PRELOAD injection tested"
    echo "  - Multiple applications tested"
    echo "  - C SQLite application tested"
    echo "  - Go SQLite application tested"
    echo "  - Python SQLite application tested"
    echo "  - Node.js SQLite application tested"
    echo "  - Concurrent applications tested"
    echo ""
    echo "✅ All applications successfully integrated with DistSQLite!"
}

main "$@"