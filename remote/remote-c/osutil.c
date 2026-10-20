/* osutil.c - platform string conversion, UTF-8 path wrappers and string buffers. */
#include "agent.h"

/* ===================================================================== */
/* Windows UTF-8 <-> UTF-16 conversion + UTF-8 path wrappers              */
/*                                                                       */
/* The wire protocol and all internal strings are UTF-8. Windows' narrow */
/* (ANSI) APIs interpret bytes through the active code page, which would */
/* corrupt non-ASCII input, so every OS boundary transcodes to UTF-16 and */
/* uses the wide-character API instead. POSIX is byte-transparent and    */
/* uses the plain standard calls.                                        */
/* ===================================================================== */

#ifdef _WIN32

wchar_t *utf8_to_wide(const char *s) {
    if (!s) return NULL;
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    if (n <= 0) return NULL;
    wchar_t *w = (wchar_t *)malloc((size_t)n * sizeof(wchar_t));
    if (!w) return NULL;
    if (MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n) <= 0) { free(w); return NULL; }
    return w;
}

char *wide_to_utf8(const wchar_t *w) {
    if (!w) return NULL;
    int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, NULL, 0, NULL, NULL);
    if (n <= 0) return NULL;
    char *s = (char *)malloc((size_t)n);
    if (!s) return NULL;
    if (WideCharToMultiByte(CP_UTF8, 0, w, -1, s, n, NULL, NULL) <= 0) { free(s); return NULL; }
    return s;
}

static char *cp_to_utf8(const char *s, UINT cp) {
    if (!s) return NULL;
    int wlen = MultiByteToWideChar(cp, 0, s, -1, NULL, 0);
    if (wlen <= 0) return xstrdup(s);
    wchar_t *w = (wchar_t *)malloc((size_t)wlen * sizeof(wchar_t));
    if (!w) return xstrdup(s);
    MultiByteToWideChar(cp, 0, s, -1, w, wlen);
    char *u = wide_to_utf8(w);
    free(w);
    return u ? u : xstrdup(s);
}

char *oem_to_utf8(const char *s) { return cp_to_utf8(s, CP_OEMCP); }

#endif /* _WIN32 */

FILE *c2a_fopen(const char *path, const char *mode) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(path);
    wchar_t *wm = utf8_to_wide(mode);
    FILE *f = (wp && wm) ? _wfopen(wp, wm) : NULL;
    free(wp);
    free(wm);
    return f;
#else
    return fopen(path, mode);
#endif
}

int c2a_remove(const char *path) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(path);
    int r = wp ? _wremove(wp) : -1;
    free(wp);
    return r;
#else
    return remove(path);
#endif
}

int c2a_rmdir(const char *path) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(path);
    int r = (wp && RemoveDirectoryW(wp)) ? 0 : -1;
    free(wp);
    return r;
#else
    return rmdir(path);
#endif
}

int c2a_rename(const char *oldpath, const char *newpath) {
#ifdef _WIN32
    wchar_t *wo = utf8_to_wide(oldpath);
    wchar_t *wn = utf8_to_wide(newpath);
    int r = (wo && wn) ? _wrename(wo, wn) : -1;
    free(wo);
    free(wn);
    return r;
#else
    return rename(oldpath, newpath);
#endif
}

int c2a_mkdir(const char *path) {
#ifdef _WIN32
    wchar_t *wp = utf8_to_wide(path);
    int r = wp ? _wmkdir(wp) : -1;
    free(wp);
    return r;
#else
    return mkdir(path, 0755);
#endif
}

char *c2a_getcwd(void) {
#ifdef _WIN32
    DWORD cap = GetCurrentDirectoryW(0, NULL);
    if (cap == 0) return NULL;
    wchar_t *w = (wchar_t *)malloc((size_t)cap * sizeof(wchar_t));
    if (!w) return NULL;
    DWORD n = GetCurrentDirectoryW(cap, w);
    char *s = (n == 0) ? NULL : wide_to_utf8(w);
    free(w);
    return s;
#else
    char buf[PROTO_MAX_PATH];
    if (!getcwd(buf, sizeof buf)) return NULL;
    return xstrdup(buf);
#endif
}

void sb_init(strbuf_t *b) {
    b->data = (char *)malloc(128);
    if (b->data) { b->data[0] = 0; }
    b->len = 0;
    b->cap = b->data ? 128 : 0;
}

void sb_free(strbuf_t *b) { free(b->data); b->data = NULL; b->len = b->cap = 0; }

int sb_append(strbuf_t *b, const char *d, size_t n) {
    if (b->len + n + 1 > b->cap) {
        size_t nc = b->cap ? b->cap : 128;
        while (nc < b->len + n + 1) nc *= 2;
        char *nd = (char *)realloc(b->data, nc);
        if (!nd) return -1;
        b->data = nd;
        b->cap = nc;
    }
    if (n) memcpy(b->data + b->len, d, n);
    b->len += n;
    b->data[b->len] = 0;
    return 0;
}

/* Append while bounding retained output. Bytes beyond MAX_STORED_OUTPUT are
 * discarded, and *truncated is set so callers can append the marker. */
void sb_append_capped(strbuf_t *b, const char *d, size_t n, int *truncated) {
    if (*truncated) return;
    size_t room = b->len < MAX_STORED_OUTPUT ? MAX_STORED_OUTPUT - b->len : 0;
    if (n > room) {
        if (room) sb_append(b, d, room);
        *truncated = 1;
        return;
    }
    sb_append(b, d, n);
}

int sb_printf(strbuf_t *b, const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    char tmp[2048];
    int n = vsnprintf(tmp, sizeof tmp, fmt, ap);
    va_end(ap);
    if (n < 0) return -1;
    if ((size_t)n < sizeof tmp) return sb_append(b, tmp, (size_t)n);
    /* very large formatted chunk: build dynamically */
    va_list ap2;
    va_start(ap2, fmt);
    char *big = (char *)malloc((size_t)n + 1);
    if (!big) { va_end(ap2); return -1; }
    vsnprintf(big, (size_t)n + 1, fmt, ap2);
    va_end(ap2);
    int rc = sb_append(b, big, (size_t)n);
    free(big);
    return rc;
}

char *sb_take(strbuf_t *b) {
    char *d = b->data ? b->data : xstrdup("");
    b->data = NULL;
    b->len = b->cap = 0;
    return d;
}
