/* crypto.c - SHA-256, ChaCha20 and per-connection stream encryption. */
#include "agent.h"

static int g_encrypted = 0;

/* ===================================================================== */
/* SHA-256 (self-contained, public domain style)                          */
/* ===================================================================== */

static const uint32_t SHA256_K[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5,
    0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
    0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc,
    0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
    0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
    0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3,
    0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5,
    0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
    0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
};

static uint32_t sha256_rotr(uint32_t x, int n) {
    return (x >> n) | (x << (32 - n));
}

static void sha256_transform(uint32_t state[8], const uint8_t data[64]) {
    uint32_t w[64];
    int i;
    for (i = 0; i < 16; i++) {
        w[i] = ((uint32_t)data[i * 4] << 24) | ((uint32_t)data[i * 4 + 1] << 16)
             | ((uint32_t)data[i * 4 + 2] << 8) | (uint32_t)data[i * 4 + 3];
    }
    for (i = 16; i < 64; i++) {
        uint32_t s0 = sha256_rotr(w[i - 15], 7) ^ sha256_rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
        uint32_t s1 = sha256_rotr(w[i - 2], 17) ^ sha256_rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3];
    uint32_t e = state[4], f = state[5], g = state[6], h = state[7];
    for (i = 0; i < 64; i++) {
        uint32_t S1 = sha256_rotr(e, 6) ^ sha256_rotr(e, 11) ^ sha256_rotr(e, 25);
        uint32_t ch = (e & f) ^ (~e & g);
        uint32_t t1 = h + S1 + ch + SHA256_K[i] + w[i];
        uint32_t S0 = sha256_rotr(a, 2) ^ sha256_rotr(a, 13) ^ sha256_rotr(a, 22);
        uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        uint32_t t2 = S0 + maj;
        h = g; g = f; f = e; e = d + t1; d = c; c = b; b = a; a = t1 + t2;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
    state[4] += e; state[5] += f; state[6] += g; state[7] += h;
}

static void sha256_digest_raw(const uint8_t *data, size_t len, uint32_t state[8]) {
    state[0] = 0x6a09e667; state[1] = 0xbb67ae85;
    state[2] = 0x3c6ef372; state[3] = 0xa54ff53a;
    state[4] = 0x510e527f; state[5] = 0x9b05688c;
    state[6] = 0x1f83d9ab; state[7] = 0x5be0cd19;
    uint64_t bitlen = (uint64_t)len * 8;
    uint8_t block[64];
    const uint8_t *p = data;
    size_t left = len;

    /* process full 64-byte blocks */
    while (left >= 64) {
        sha256_transform(state, p);
        p += 64;
        left -= 64;
    }

    /* final block: pad with 0x80, zeros, then 8-byte big-endian bit length */
    memset(block, 0, sizeof block);
    memcpy(block, p, left);
    block[left] = 0x80;
    if (left >= 56) {
        sha256_transform(state, block);
        memset(block, 0, sizeof block);
    }
    for (int i = 0; i < 8; i++) {
        block[63 - i] = (uint8_t)(bitlen >> (i * 8));
    }
    sha256_transform(state, block);
}

void sha256_digest(const void *data, size_t len, uint8_t out[32]) {
    uint32_t state[8];
    sha256_digest_raw((const uint8_t *)data, len, state);
    for (int i = 0; i < 8; i++) {
        out[i * 4]     = (uint8_t)(state[i] >> 24);
        out[i * 4 + 1] = (uint8_t)(state[i] >> 16);
        out[i * 4 + 2] = (uint8_t)(state[i] >> 8);
        out[i * 4 + 3] = (uint8_t)(state[i]);
    }
}

void sha256_hex(const void *data, size_t len, char out[65]) {
    uint8_t d[32];
    sha256_digest(data, len, d);
    static const char hexc[] = "0123456789abcdef";
    for (int i = 0; i < 32; i++) {
        out[i * 2]     = hexc[d[i] >> 4];
        out[i * 2 + 1] = hexc[d[i] & 0xf];
    }
    out[64] = 0;
}

/* ===================================================================== */
/* ChaCha20 (RFC 7539: 256-bit key, 96-bit nonce, 32-bit counter)        */
/* Adapted from CycloneCRYPTO Open chacha20 (样例代码/chacha20.c).        */
/* ===================================================================== */

typedef struct {
    uint32_t state[16];
    uint8_t keystream[64];
    size_t pos;
} Chacha20Context;

static uint32_t c20_load32le(const uint8_t *p) {
    return ((uint32_t)p[0]) | ((uint32_t)p[1] << 8)
         | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static void c20_store32le(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)v; p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16); p[3] = (uint8_t)(v >> 24);
}

static uint32_t c20_rol(uint32_t x, int n) {
    return (x << n) | (x >> (32 - n));
}

#define C20_QR(a, b, c, d) do {                     \
    (a) += (b); (d) ^= (a); (d) = c20_rol((d), 16); \
    (c) += (d); (b) ^= (c); (b) = c20_rol((b), 12); \
    (a) += (b); (d) ^= (a); (d) = c20_rol((d), 8);  \
    (c) += (d); (b) ^= (c); (b) = c20_rol((b), 7);  \
} while (0)

static void c20_block(Chacha20Context *ctx) {
    uint32_t w[16];
    int i;
    for (i = 0; i < 16; i++) w[i] = ctx->state[i];
    for (i = 0; i < 10; i++) {
        C20_QR(w[0], w[4], w[8], w[12]);
        C20_QR(w[1], w[5], w[9], w[13]);
        C20_QR(w[2], w[6], w[10], w[14]);
        C20_QR(w[3], w[7], w[11], w[15]);
        C20_QR(w[0], w[5], w[10], w[15]);
        C20_QR(w[1], w[6], w[11], w[12]);
        C20_QR(w[2], w[7], w[8], w[13]);
        C20_QR(w[3], w[4], w[9], w[14]);
    }
    for (i = 0; i < 16; i++) {
        w[i] += ctx->state[i];
        c20_store32le(ctx->keystream + i * 4, w[i]);
    }
    ctx->state[12]++;
    if (ctx->state[12] == 0) ctx->state[13]++;
    ctx->pos = 0;
}

static void c20_init(Chacha20Context *ctx, const uint8_t key[32], const uint8_t nonce[12]) {
    static const uint32_t consts[4] = { 0x61707865U, 0x3320646EU, 0x79622D32U, 0x6B206574U };
    int i;
    ctx->state[0] = consts[0]; ctx->state[1] = consts[1];
    ctx->state[2] = consts[2]; ctx->state[3] = consts[3];
    for (i = 0; i < 8; i++) ctx->state[4 + i] = c20_load32le(key + i * 4);
    ctx->state[12] = 0;
    ctx->state[13] = c20_load32le(nonce);
    ctx->state[14] = c20_load32le(nonce + 4);
    ctx->state[15] = c20_load32le(nonce + 8);
    ctx->pos = 0;
}

static void c20_crypt(Chacha20Context *ctx, const uint8_t *in, uint8_t *out, size_t n) {
    size_t off = 0;
    while (off < n) {
        if (ctx->pos == 0 || ctx->pos >= 64) c20_block(ctx);
        size_t take = (n - off) < (64 - ctx->pos) ? (n - off) : (64 - ctx->pos);
        for (size_t i = 0; i < take; i++) out[off + i] = in[off + i] ^ ctx->keystream[ctx->pos + i];
        ctx->pos += take;
        off += take;
    }
}

/* ===================================================================== */
/* connection-wide stream encryption state                               */
/* ===================================================================== */

static Chacha20Context g_tx, g_rx;

/* Derive the per-connection key + nonce for one direction from the shared auth
 * token and the fresh handshake nonce:
 *   key(dir)   = SHA256(nonce || token || nonce || dir)          (32 bytes)
 *   nonce(dir) = SHA256(token || nonce || token || dir)[0:12]    (12 bytes)
 * dir separates the two directions: 0x01 = C2->Agent, 0x02 = Agent->C2. */
static void crypto_derive_material(const char *token, const char *nonce,
                                   uint8_t dir, uint8_t key_out[32],
                                   uint8_t nonce_out[12]) {
    size_t tlen = strlen(token);
    size_t nlen = strlen(nonce);
    size_t maxlen = tlen + nlen + (nlen > tlen ? nlen : tlen) + 1;
    uint8_t *buf = (uint8_t *)malloc(maxlen);
    if (!buf) return;
    size_t off;

    /* key = SHA256(nonce || token || nonce || dir) */
    off = 0;
    memcpy(buf + off, nonce, nlen); off += nlen;
    memcpy(buf + off, token, tlen); off += tlen;
    memcpy(buf + off, nonce, nlen); off += nlen;
    buf[off++] = dir;
    sha256_digest(buf, off, key_out);

    /* nonce = SHA256(token || nonce || token || dir)[0:12] */
    off = 0;
    memcpy(buf + off, token, tlen); off += tlen;
    memcpy(buf + off, nonce, nlen); off += nlen;
    memcpy(buf + off, token, tlen); off += tlen;
    buf[off++] = dir;
    {
        uint8_t sum[32];
        sha256_digest(buf, off, sum);
        memcpy(nonce_out, sum, 12);
    }

    free(buf);
}

void crypto_enable_agent(const char *token, const char *nonce) {
    uint8_t tx_key[32], rx_key[32], tx_nonce[12], rx_nonce[12];
    crypto_derive_material(token, nonce, 0x02, tx_key, tx_nonce); /* agent outbound: Agent -> C2 */
    crypto_derive_material(token, nonce, 0x01, rx_key, rx_nonce); /* agent inbound : C2 -> Agent */
    c20_init(&g_tx, tx_key, tx_nonce);
    c20_init(&g_rx, rx_key, rx_nonce);
    g_encrypted = 1;
}

void crypto_disable(void) {
    g_encrypted = 0;
}

int crypto_is_enabled(void) {
    return g_encrypted;
}

void crypto_encrypt_buf(const uint8_t *in, uint8_t *out, size_t n) {
    if (!g_encrypted) { memmove(out, in, n); return; }
    c20_crypt(&g_tx, in, out, n);
}

void crypto_decrypt_buf(uint8_t *buf, size_t n) {
    if (!g_encrypted) return;
    c20_crypt(&g_rx, buf, buf, n);
}
