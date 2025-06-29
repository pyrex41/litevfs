/* DistCore Thread Safety - OCaml callback protection */

#ifndef DISTCORE_THREAD_SAFETY_H
#define DISTCORE_THREAD_SAFETY_H

#include <pthread.h>
#include <stdbool.h>

/* Global mutex for OCaml runtime protection */
extern pthread_mutex_t distcore_ocaml_mutex;
extern bool distcore_ocaml_mutex_initialized;

/* Initialize thread safety */
int distcore_thread_safety_init(void);

/* Cleanup thread safety */
void distcore_thread_safety_cleanup(void);

/* Macros for safe OCaml callbacks */
#define DISTCORE_OCAML_LOCK() \
    do { \
        if (distcore_ocaml_mutex_initialized) { \
            pthread_mutex_lock(&distcore_ocaml_mutex); \
        } \
    } while(0)

#define DISTCORE_OCAML_UNLOCK() \
    do { \
        if (distcore_ocaml_mutex_initialized) { \
            pthread_mutex_unlock(&distcore_ocaml_mutex); \
        } \
    } while(0)

/* Thread-safe OCaml callback wrapper */
#define DISTCORE_OCAML_CALLBACK(code) \
    do { \
        DISTCORE_OCAML_LOCK(); \
        code; \
        DISTCORE_OCAML_UNLOCK(); \
    } while(0)

#endif /* DISTCORE_THREAD_SAFETY_H */