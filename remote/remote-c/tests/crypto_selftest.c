/* Self-test for the C agent's per-connection ChaCha20 key/nonce derivation.
 *
 * Build (from remote/remote-c):
 *   Windows: gcc -O2 -Wall -Wextra -o crypto_selftest tests/crypto_selftest.c -lws2_32
 *   POSIX:   gcc -O2 -Wall -Wextra -o crypto_selftest tests/crypto_selftest.c -lpthread
 *
 * The harness #includes protocol.c so the file-local derive helper is visible.
 * The expected values are the cross-language interop vectors shared with the Go
 * control end and remote-go/remote-py.
 */
#include <stdio.h>
#include <string.h>

#include "../protocol.c"

static int check_hex(const char *label, const uint8_t *got, int len, const char *want) {
    static const char hc[] = "0123456789abcdef";
    char hex[160];
    for (int i = 0; i < len; i++) {
        hex[i * 2] = hc[got[i] >> 4];
        hex[i * 2 + 1] = hc[got[i] & 0xf];
    }
    hex[len * 2] = 0;
    if (strcmp(hex, want) != 0) {
        fprintf(stderr, "%s mismatch\n got: %s\nwant: %s\n", label, hex, want);
        return 1;
    }
    return 0;
}

int main(void) {
    uint8_t key[32], nonce[12];
    const char *token = "change-me-shared-token";
    const char *hn = "0123456789abcdef";

    crypto_derive_material(token, hn, 0x01, key, nonce);
    if (check_hex("dir1 key", key, 32,
                  "ea42424fcb14a1a3395d2f013866be2c1c8ab812b8ecd29790f6bda98c4a3dbe")) return 1;
    if (check_hex("dir1 nonce", nonce, 12, "e6d636597a46b43b686f7aa7")) return 1;

    crypto_derive_material(token, hn, 0x02, key, nonce);
    if (check_hex("dir2 key", key, 32,
                  "a652964193b67a6d0d2b0787e116c2f05133a1987fb5536df1c4c1f87a64f2ad")) return 1;
    if (check_hex("dir2 nonce", nonce, 12, "5d88656aeb8a77320e65609f")) return 1;

    printf("crypto_selftest: OK\n");
    return 0;
}
