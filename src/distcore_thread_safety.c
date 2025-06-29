/* DistCore Thread Safety Implementation */

#include "distcore_thread_safety.h"
#include "distcore_errors.h"
#include <stdio.h>

/* Global mutex for OCaml runtime protection */
pthread_mutex_t distcore_ocaml_mutex = PTHREAD_MUTEX_INITIALIZER;
bool distcore_ocaml_mutex_initialized = false;

/* Initialize thread safety */
int distcore_thread_safety_init(void) {
    if (distcore_ocaml_mutex_initialized) {
        return DISTCORE_ERR_ALREADY_INIT;
    }
    
    pthread_mutexattr_t attr;
    int rc = pthread_mutexattr_init(&attr);
    if (rc != 0) {
        fprintf(stderr, "[DistCore] Failed to init mutex attributes: %d\n", rc);
        return DISTCORE_ERR_LOCK_FAILED;
    }
    
    /* Set mutex to be recursive to handle nested OCaml calls */
    rc = pthread_mutexattr_settype(&attr, PTHREAD_MUTEX_RECURSIVE);
    if (rc != 0) {
        pthread_mutexattr_destroy(&attr);
        fprintf(stderr, "[DistCore] Failed to set mutex type: %d\n", rc);
        return DISTCORE_ERR_LOCK_FAILED;
    }
    
    rc = pthread_mutex_init(&distcore_ocaml_mutex, &attr);
    pthread_mutexattr_destroy(&attr);
    
    if (rc != 0) {
        fprintf(stderr, "[DistCore] Failed to init OCaml mutex: %d\n", rc);
        return DISTCORE_ERR_LOCK_FAILED;
    }
    
    distcore_ocaml_mutex_initialized = true;
    printf("[DistCore] Thread safety initialized\n");
    return DISTCORE_OK;
}

/* Cleanup thread safety */
void distcore_thread_safety_cleanup(void) {
    if (distcore_ocaml_mutex_initialized) {
        pthread_mutex_destroy(&distcore_ocaml_mutex);
        distcore_ocaml_mutex_initialized = false;
        printf("[DistCore] Thread safety cleaned up\n");
    }
}