import sqlite3
import random
import string
import sys

def generate_large_data(size_mb=5):
    """Generate test data to create a database of approximately size_mb"""
    conn = sqlite3.connect('test_large.db')
    cursor = conn.cursor()
    
    # Create tables
    cursor.execute('''CREATE TABLE IF NOT EXISTS large_test (
        id INTEGER PRIMARY KEY,
        data TEXT,
        payload BLOB,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
    )''')
    
    # Calculate rows needed for target size
    target_bytes = size_mb * 1024 * 1024
    row_size = 1024  # ~1KB per row
    rows_needed = target_bytes // row_size
    
    print(f"Generating {rows_needed} rows of ~{row_size} bytes each...")
    
    # Insert large amount of data
    batch_size = 1000
    for batch in range(0, rows_needed, batch_size):
        batch_data = []
        for i in range(batch_size):
            if batch + i >= rows_needed:
                break
            data = ''.join(random.choices(string.ascii_letters + string.digits, k=500))
            payload = random.randbytes(512)
            batch_data.append((batch + i, data, payload))
        
        cursor.executemany(
            "INSERT INTO large_test (id, data, payload) VALUES (?, ?, ?)",
            batch_data
        )
        
        if batch % 10000 == 0:
            print(f"Inserted {batch + batch_size} rows...")
            conn.commit()
    
    conn.commit()
    
    # Get final stats
    cursor.execute("SELECT COUNT(*) FROM large_test")
    count = cursor.fetchone()[0]
    
    cursor.execute("SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()")
    size_bytes = cursor.fetchone()[0]
    size_mb = size_bytes / (1024 * 1024)
    
    print(f"✅ Created database with {count} rows, {size_mb:.2f}MB")
    
    conn.close()
    return count, size_bytes

if __name__ == "__main__":
    try:
        generate_large_data(5)  # 5MB database
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
