/* SPDX-License-Identifier: Apache-2.0 */
#include "sha256.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Failure injection is compiled into a separate test object, never the tools. */
const char *test_gcry_check_version(const char *version)
{
    return getenv("TEST_SHA_INIT_FAIL") ? NULL : gcry_check_version(version);
}

gcry_error_t test_gcry_md_open(gcry_md_hd_t *handle, int algo, unsigned int flags)
{
    return getenv("TEST_SHA_OPEN_FAIL") ? 1 : gcry_md_open(handle, algo, flags);
}

int main(void)
{
    static const size_t sizes[] = {0, 1, 55, 56, 63, 64, 65, 4095, 4096, 4097, 1048576};
    unsigned char *input = malloc(sizes[10]), one[32], stream[32];
    struct sha256_state state;
    size_t i, j, offset, length;
    if (!input)
        return 1;
    for (i = 0; i < sizes[10]; ++i)
        input[i] = (unsigned char)(i * 17 + i / 13);
    /* Exercise initialization through the streaming API before one-shot use. */
    erofs_sha256_init(&state);
    if (erofs_sha256_process(&state, (const unsigned char *)"abc", 3) ||
        erofs_sha256_done(&state, stream))
        return 2;
    for (j = 0; j < 32; ++j)
        printf("%02x", stream[j]);
    putchar('\n');
    for (i = 0; i < sizeof(sizes) / sizeof(sizes[0]); ++i) {
        erofs_sha256(input, sizes[i], one);
        erofs_sha256_init(&state);
        for (offset = 0; offset < sizes[i]; offset += length) {
            length = sizes[i] - offset;
            if (length > 137)
                length = 137;
            if (erofs_sha256_process(&state, input + offset, length))
                return 3;
        }
        if (erofs_sha256_done(&state, stream) || memcmp(one, stream, 32) ||
            erofs_sha256_done(&state, stream) != -1 ||
            erofs_sha256_process(&state, input, 1) != -1)
            return 4;
        for (j = 0; j < 32; ++j)
            printf("%02x", one[j]);
        putchar('\n');
    }
    free(input);
    return 0;
}
