/* Simple main function to create a complete OCaml executable */
#include <caml/mlvalues.h>
#include <caml/callback.h>

extern int distcore_init(void);

int main(int argc, char **argv) {
    /* This creates a complete OCaml executable with all symbols */
    distcore_init();
    return 0;
}