#include <stdio.h>
#include <sqlite3.h>

int main() {
    printf("SQLite version: %s\n", sqlite3_libversion());
    
    sqlite3_vfs *vfs = sqlite3_vfs_find(NULL);
    if (vfs) {
        printf("Default VFS: %s\n", vfs->zName);
    }
    
    return 0;
}
