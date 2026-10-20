/* pathutil.c - filesystem path helpers, host info, timing and keepalive. */
#include "agent.h"
#include <ctype.h>

/* ===================================================================== */
/* misc helpers                                                          */
/* ===================================================================== */

int path_exists(const char *p) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(p);
    DWORD a = wp ? GetFileAttributesW(wp) : INVALID_FILE_ATTRIBUTES;
    free(wp);
    return a != INVALID_FILE_ATTRIBUTES;
#else
    struct stat st;
    return stat(p, &st) == 0;
#endif
}

int is_dir(const char *p) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(p);
    DWORD a = wp ? GetFileAttributesW(wp) : INVALID_FILE_ATTRIBUTES;
    free(wp);
    if (a == INVALID_FILE_ATTRIBUTES) return 0;
    return (a & FILE_ATTRIBUTE_DIRECTORY) != 0;
#else
    struct stat st;
    if (stat(p, &st) != 0) return 0;
    return S_ISDIR(st.st_mode);
#endif
}

static int mkdir_one(const char *p) {
    return c2a_mkdir(p);
}
void mkdir_p(const char *path) {
    char tmp[PROTO_MAX_PATH];
    size_t len = strlen(path);
    if (len == 0 || len >= sizeof tmp) return;
    memcpy(tmp, path, len + 1);
    /* drop trailing separator */
    while (len > 1 && (tmp[len - 1] == '/' || tmp[len - 1] == '\\')) tmp[--len] = 0;
    char *p = tmp;
    if (len >= 2 && tmp[1] == ':') p = tmp + 2; /* skip "C:" */
    if (*p == '/' || *p == '\\') p++;           /* skip leading sep */
    for (; *p; p++) {
        if (*p == '/' || *p == '\\') {
            char saved = *p;
            *p = 0;
            mkdir_one(tmp);
            *p = saved;
        }
    }
    mkdir_one(tmp);
}

void make_parent_dirs(const char *path) {
    char tmp[PROTO_MAX_PATH];
    size_t len = strlen(path);
    if (len == 0 || len >= sizeof tmp) return;
    memcpy(tmp, path, len + 1);
    char *sep = NULL;
    for (char *p = tmp; *p; p++) if (*p == '/' || *p == '\\') sep = p;
    if (sep) {
        *sep = 0;
        mkdir_p(tmp);
    }
}

/* Normalize a path to an absolute, slash-separated, dot-free form. The result
 * is used only for lexical containment checks (copy destination inside source). */
static void normalize_abs_path(const char *in, char *out, size_t outsz) {
    char tmp[PROTO_MAX_PATH * 2];
    char abs[PROTO_MAX_PATH * 4];
    char drive[3] = {0, 0, 0};
    if (!in || !*in || outsz == 0) { if (outsz) out[0] = 0; return; }
    snprintf(tmp, sizeof tmp, "%s", in);
    for (char *q = tmp; *q; q++) if (*q == '\\') *q = '/';

    char *p = tmp;
    if (isalpha((unsigned char)p[0]) && p[1] == ':') {
        drive[0] = p[0];
        drive[1] = ':';
        p += 2;
    }
    if (*p != '/') {
        char cwd[PROTO_MAX_PATH];
        cwd[0] = 0;
        char *raw_cwd = c2a_getcwd();
        if (raw_cwd) {
            size_t cl = strlen(raw_cwd);
            if (cl >= sizeof cwd) cl = sizeof cwd - 1;
            memcpy(cwd, raw_cwd, cl);
            cwd[cl] = 0;
            free(raw_cwd);
        }
        for (char *q = cwd; *q; q++) if (*q == '\\') *q = '/';
        if (!drive[0] && isalpha((unsigned char)cwd[0]) && cwd[1] == ':') {
            drive[0] = cwd[0];
            drive[1] = ':';
        }
        snprintf(abs, sizeof abs, "%s/%s", cwd, p);
        p = (drive[0] && isalpha((unsigned char)abs[0]) && abs[1] == ':') ? abs + 2 : abs;
    }

    size_t cap = strlen(p) / 2 + 4;
    char **parts = (char **)malloc(cap * sizeof(char *));
    if (!parts) { out[0] = 0; return; }
    int n = 0;
    for (char *tok = strtok(p, "/"); tok; tok = strtok(NULL, "/")) {
        if (!strcmp(tok, ".")) continue;
        if (!strcmp(tok, "..")) {
            if (n > 0) n--;
            continue;
        }
        if ((size_t)n < cap) parts[n++] = tok;
    }

    size_t off = 0;
    if (drive[0] && off + 2 < outsz) {
        out[off++] = drive[0];
        out[off++] = drive[1];
    }
    if (off < outsz - 1) out[off++] = '/';
    for (int i = 0; i < n && off < outsz - 1; i++) {
        if (i > 0 && off < outsz - 1) out[off++] = '/';
        size_t l = strlen(parts[i]);
        if (l > outsz - 1 - off) l = outsz - 1 - off;
        memcpy(out + off, parts[i], l);
        off += l;
    }
    free(parts);
    out[off] = 0;
}

int path_is_inside(const char *parent, const char *child) {
    char p[PROTO_MAX_PATH * 2], c[PROTO_MAX_PATH * 2];
    normalize_abs_path(parent, p, sizeof p);
    normalize_abs_path(child, c, sizeof c);
    size_t plen = strlen(p);
    if (plen == 0) return 0;
    if (plen > 0 && p[plen - 1] == '/') {
#ifdef _WIN32
        return _strnicmp(p, c, plen) == 0;
#else
        return strncmp(p, c, plen) == 0;
#endif
    }
#ifdef _WIN32
    if (_strnicmp(p, c, plen) != 0) return 0;
#else
    if (strncmp(p, c, plen) != 0) return 0;
#endif
    return c[plen] == 0 || c[plen] == '/';
}

char *detect_os(void) {
#ifdef _WIN32
    return xstrdup("Windows");
#elif defined(__APPLE__)
    return xstrdup("macOS");
#else
    return xstrdup("Linux");
#endif
}

int get_hostname(char *buf, size_t n) {
#ifdef _WIN32
    wchar_t wbuf[256];
    DWORD sz = (DWORD)(sizeof wbuf / sizeof wbuf[0]);
    if (!GetComputerNameW(wbuf, &sz)) return -1;
    char *u = wide_to_utf8(wbuf);
    if (!u) return -1;
    snprintf(buf, n, "%s", u);
    free(u);
    return 0;
#else
    if (gethostname(buf, n) != 0) return -1;
    buf[n - 1] = 0;
    return 0;
#endif
}

int64_t now_ms(void) {
#ifdef _WIN32
    return (int64_t)GetTickCount64();
#else
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
#endif
}

void sleep_sec(double s) {
#ifdef _WIN32
    Sleep((DWORD)(s * 1000.0));
#else
    struct timespec ts;
    ts.tv_sec = (time_t)s;
    ts.tv_nsec = (long)((s - (double)ts.tv_sec) * 1e9);
    nanosleep(&ts, NULL);
#endif
}

void set_keepalive(sockfd_t sock) {
#ifdef _WIN32
    struct tcp_keepalive ka;
    BOOL on = TRUE;
    DWORD bytes = 0;
    ka.onoff = 1;
    ka.keepalivetime = 30000;     /* ms */
    ka.keepaliveinterval = 10000; /* ms */
    (void)setsockopt(sock, SOL_SOCKET, SO_KEEPALIVE, (const char *)&on, sizeof on);
    WSAIoctl(sock, SIO_KEEPALIVE_VALS, &ka, sizeof ka, NULL, 0, &bytes, NULL, NULL);
#else
    int on = 1;
    (void)setsockopt(sock, SOL_SOCKET, SO_KEEPALIVE, &on, sizeof on);
#ifdef TCP_KEEPIDLE
    {
        int idle = 30, intvl = 10, cnt = 3;
        (void)setsockopt(sock, IPPROTO_TCP, TCP_KEEPIDLE, &idle, sizeof idle);
        (void)setsockopt(sock, IPPROTO_TCP, TCP_KEEPINTVL, &intvl, sizeof intvl);
        (void)setsockopt(sock, IPPROTO_TCP, TCP_KEEPCNT, &cnt, sizeof cnt);
    }
#endif
#endif
}
