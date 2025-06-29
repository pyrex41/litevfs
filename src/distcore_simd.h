/* DistCore SIMD Operations - Runtime CPU detection */

#ifndef DISTCORE_SIMD_H
#define DISTCORE_SIMD_H

#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>

/* CPU feature flags */
typedef struct {
    bool has_sse2;
    bool has_avx2;
    bool has_avx512f;
} distcore_cpu_features_t;

/* Get CPU features */
const distcore_cpu_features_t* distcore_get_cpu_features(void);

/* Function pointers for SIMD operations */
typedef int (*distcore_memcmp_fn)(const void*, const void*, size_t);
typedef void* (*distcore_memcpy_fn)(void*, const void*, size_t);
typedef uint32_t (*distcore_hash_fn)(const void*, size_t);

/* Get optimized function implementations */
distcore_memcmp_fn distcore_get_memcmp(void);
distcore_memcpy_fn distcore_get_memcpy(void);
distcore_hash_fn distcore_get_hash(void);

/* Implementations */
int distcore_memcmp_scalar(const void* s1, const void* s2, size_t n);
void* distcore_memcpy_scalar(void* dest, const void* src, size_t n);
uint32_t distcore_hash_scalar(const void* data, size_t len);

#ifdef __SSE2__
int distcore_memcmp_sse2(const void* s1, const void* s2, size_t n);
void* distcore_memcpy_sse2(void* dest, const void* src, size_t n);
uint32_t distcore_hash_sse2(const void* data, size_t len);
#endif

#ifdef __AVX2__
int distcore_memcmp_avx2(const void* s1, const void* s2, size_t n);
void* distcore_memcpy_avx2(void* dest, const void* src, size_t n);
uint32_t distcore_hash_avx2(const void* data, size_t len);
#endif

#endif /* DISTCORE_SIMD_H */