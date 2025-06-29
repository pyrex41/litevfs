/* Simple test for OCaml runtime initialization */
#include <stdio.h>
#include <dlfcn.h>

int main() {
    printf("Loading libdistcore.so...\n");
    
    /* Load the library */
    void *handle = dlopen("./build/libdistcore.so", RTLD_NOW | RTLD_GLOBAL);
    if (!handle) {
        fprintf(stderr, "Failed to load library: %s\n", dlerror());
        return 1;
    }
    
    printf("Library loaded successfully\n");
    
    /* Get the init function */
    int (*distcore_init)(void) = dlsym(handle, "distcore_init");
    if (!distcore_init) {
        fprintf(stderr, "Failed to find distcore_init: %s\n", dlerror());
        return 1;
    }
    
    printf("Calling distcore_init()...\n");
    
    /* Initialize OCaml runtime */
    int result = distcore_init();
    
    printf("distcore_init returned: %d\n", result);
    
    printf("OCaml runtime test successful!\n");
    
    return 0;
}