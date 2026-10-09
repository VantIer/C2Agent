/* actions.c - local file system actions executed on the remote Agent.
 * Result strings follow the Python agent's exact wording so the C2/LLM
 * behavior stays consistent. Errors are reported as "Error: ..." strings.
 */
#include "agent.h"

/* ===================================================================== */
/* helpers                                                               */
/* ===================================================================== */

static char *slurp(const char *path, size_t *out_len) {
    FILE *f = c2a_fopen(path, "rb");
    if (!f) return NULL;
    if (fseek(f, 0, SEEK_END) != 0) { fclose(f); return NULL; }
    long sz = ftell(f);
    if (sz < 0) { fclose(f); return NULL; }
    if (fseek(f, 0, SEEK_SET) != 0) { fclose(f); return NULL; }
    char *buf = (char *)malloc((size_t)sz + 1);
    if (!buf) { fclose(f); return NULL; }
    size_t got = fread(buf, 1, (size_t)sz, f);
    fclose(f);
    buf[got] = 0;
    *out_len = got;
    return buf;
}

static int copy_file(const char *src, const char *dest) {
    FILE *fin = c2a_fopen(src, "rb");
    if (!fin) return -1;
    FILE *fout = c2a_fopen(dest, "wb");
    if (!fout) { fclose(fin); return -1; }
    char buf[65536];
    size_t n;
    while ((n = fread(buf, 1, sizeof buf, fin)) > 0) {
        if (fwrite(buf, 1, n, fout) != n) { fclose(fin); fclose(fout); return -1; }
    }
    fclose(fin);
    fclose(fout);
    return 0;
}

#ifdef _WIN32

/* Build L"<dir>\*" into a fixed buffer. Returns 0 on success. */
static int wide_glob(const wchar_t *dir, wchar_t *out, size_t cap) {
    size_t dl = wcslen(dir);
    if (dl + 3 > cap) return -1;
    memcpy(out, dir, dl * sizeof(wchar_t));
    out[dl] = L'\\';
    out[dl + 1] = L'*';
    out[dl + 2] = 0;
    return 0;
}

static int rmtree(const char *path) {
    wchar_t *wpath = utf8_to_wide(path);
    if (!wpath) return -1;
    wchar_t pattern[PROTO_MAX_PATH];
    int have_glob = (wide_glob(wpath, pattern, PROTO_MAX_PATH) == 0);
    free(wpath);
    int rc = 0;
    if (have_glob) {
        WIN32_FIND_DATAW fd;
        HANDLE h = FindFirstFileW(pattern, &fd);
        if (h != INVALID_HANDLE_VALUE) {
            do {
                if (!wcscmp(fd.cFileName, L".") || !wcscmp(fd.cFileName, L"..")) continue;
                char *name = wide_to_utf8(fd.cFileName);
                if (!name) { rc = -1; continue; }
                char full[PROTO_MAX_PATH];
                snprintf(full, sizeof full, "%s\\%s", path, name);
                free(name);
                if (fd.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) {
                    if (rmtree(full) != 0) rc = -1;
                } else if (c2a_remove(full) != 0) {
                    rc = -1;
                }
            } while (FindNextFileW(h, &fd));
            FindClose(h);
        }
    }
    if (c2a_rmdir(path) != 0) rc = -1;
    return rc;
}

static int copy_tree(const char *src, const char *dest) {
    mkdir_p(dest);
    wchar_t *wsrc = utf8_to_wide(src);
    if (!wsrc) return -1;
    wchar_t pattern[PROTO_MAX_PATH];
    int have_glob = (wide_glob(wsrc, pattern, PROTO_MAX_PATH) == 0);
    free(wsrc);
    if (!have_glob) return -1;
    WIN32_FIND_DATAW fd;
    HANDLE h = FindFirstFileW(pattern, &fd);
    if (h == INVALID_HANDLE_VALUE) return -1;
    do {
        if (!wcscmp(fd.cFileName, L".") || !wcscmp(fd.cFileName, L"..")) continue;
        char *name = wide_to_utf8(fd.cFileName);
        if (!name) { FindClose(h); return -1; }
        char sp[PROTO_MAX_PATH], dp[PROTO_MAX_PATH];
        snprintf(sp, sizeof sp, "%s\\%s", src, name);
        snprintf(dp, sizeof dp, "%s\\%s", dest, name);
        free(name);
        if (fd.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) {
            if (copy_tree(sp, dp) != 0) { FindClose(h); return -1; }
        } else if (copy_file(sp, dp) != 0) { FindClose(h); return -1; }
    } while (FindNextFileW(h, &fd));
    FindClose(h);
    return 0;
}

static char *act_list_dir(const char *path) {
    wchar_t *wpath = utf8_to_wide(path);
    wchar_t pattern[PROTO_MAX_PATH];
    int have_glob = wpath && (wide_glob(wpath, pattern, PROTO_MAX_PATH) == 0);
    free(wpath);
    WIN32_FIND_DATAW fd;
    HANDLE h = have_glob ? FindFirstFileW(pattern, &fd) : INVALID_HANDLE_VALUE;
    if (h == INVALID_HANDLE_VALUE) {
        if (!path_exists(path)) return printf_str("Path does not exist: %s", path);
        if (!is_dir(path)) return printf_str("%s is a file", path);
        return xstrdup("Empty directory");
    }
    strbuf_t out;
    sb_init(&out);
    int count = 0;
    do {
        if (!wcscmp(fd.cFileName, L".") || !wcscmp(fd.cFileName, L"..")) continue;
        char *name = wide_to_utf8(fd.cFileName);
        if (!name) continue;
        int isd = (fd.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
        unsigned long long sz = isd ? 0ULL
            : (((unsigned long long)fd.nFileSizeHigh << 32) | (unsigned long long)fd.nFileSizeLow);
        sb_printf(&out, "%s %12" C2A_ULL " %s\n", isd ? "DIR" : "FILE", sz, name);
        free(name);
        count++;
    } while (FindNextFileW(h, &fd));
    FindClose(h);
    if (count == 0) { sb_free(&out); return xstrdup("Empty directory"); }
    return sb_take(&out);
}

#else /* POSIX */

static int rmtree(const char *path) {
    DIR *d = opendir(path);
    if (!d) return -1;
    struct dirent *de;
    int rc = 0;
    while ((de = readdir(d)) != NULL) {
        if (!strcmp(de->d_name, ".") || !strcmp(de->d_name, "..")) continue;
        char full[PROTO_MAX_PATH];
        snprintf(full, sizeof full, "%s/%s", path, de->d_name);
        struct stat st;
        if (lstat(full, &st) == 0) {
            if (S_ISDIR(st.st_mode) && !S_ISLNK(st.st_mode)) {
                if (rmtree(full) != 0) rc = -1;
            } else if (remove(full) != 0) {
                rc = -1;
            }
        }
    }
    closedir(d);
    if (remove(path) != 0) rc = -1;
    return rc;
}

static int copy_tree(const char *src, const char *dest) {
    mkdir_p(dest);
    DIR *d = opendir(src);
    if (!d) return -1;
    struct dirent *de;
    while ((de = readdir(d)) != NULL) {
        if (!strcmp(de->d_name, ".") || !strcmp(de->d_name, "..")) continue;
        char sp[PROTO_MAX_PATH], dp[PROTO_MAX_PATH];
        snprintf(sp, sizeof sp, "%s/%s", src, de->d_name);
        snprintf(dp, sizeof dp, "%s/%s", dest, de->d_name);
        struct stat st;
        if (stat(sp, &st) == 0 && S_ISDIR(st.st_mode)) {
            if (copy_tree(sp, dp) != 0) { closedir(d); return -1; }
        } else if (copy_file(sp, dp) != 0) { closedir(d); return -1; }
    }
    closedir(d);
    return 0;
}

static char *act_list_dir(const char *path) {
    DIR *d = opendir(path);
    if (!d) {
        if (!path_exists(path)) return printf_str("Path does not exist: %s", path);
        if (!is_dir(path)) return printf_str("%s is a file", path);
        return xstrdup("Empty directory");
    }
    strbuf_t out;
    sb_init(&out);
    int count = 0;
    struct dirent *de;
    while ((de = readdir(d)) != NULL) {
        if (!strcmp(de->d_name, ".") || !strcmp(de->d_name, "..")) continue;
        char full[PROTO_MAX_PATH];
        snprintf(full, sizeof full, "%s/%s", path, de->d_name);
        struct stat st;
        int isd = 0;
        unsigned long long sz = 0;
        if (stat(full, &st) == 0) {
            isd = S_ISDIR(st.st_mode);
            if (!isd) sz = (unsigned long long)st.st_size;
        }
        sb_printf(&out, "%s %12" C2A_ULL " %s\n", isd ? "DIR" : "FILE", sz, de->d_name);
        count++;
    }
    closedir(d);
    if (count == 0) { sb_free(&out); return xstrdup("Empty directory"); }
    return sb_take(&out);
}

#endif

static void path_with_name(const char *path, const char *new_name, char *out, size_t outsz) {
    /* parent dir (kept verbatim, including trailing separator) + new_name.
       new_name is a plain name; the Python agent joins it onto the parent. */
    size_t len = strlen(path);
    size_t i = len;
    while (i > 0 && path[i - 1] != '/' && path[i - 1] != '\\') i--;
    if (i == 0) {
        snprintf(out, outsz, "%s", new_name);
    } else {
        size_t dirlen = i;
        if (dirlen >= outsz) dirlen = outsz - 1;
        memcpy(out, path, dirlen);
        out[dirlen] = 0;
        strncat(out, new_name, outsz - dirlen - 1);
    }
}

/* ===================================================================== */
/* actions                                                               */
/* ===================================================================== */

static char *act_get_cwd(void) {
    char *cwd = c2a_getcwd();
    if (!cwd) return printf_str("Error getting cwd: %s", "failed");
    return cwd;
}

static char *act_make_dir(char **p, int n) {
    if (n < 1) return xstrdup("Error: missing make_dir path");
    const char *path = p[0];
    if (path_exists(path)) return printf_str("Directory already exists: %s", path);
    mkdir_p(path);
    if (!path_exists(path)) return printf_str("Error creating directory: %s", "failed");
    return printf_str("Successfully created directory: %s", path);
}

static char *act_create_file(char **p, int n) {
    if (n < 1) return xstrdup("Error: missing create_file path");
    const char *path = p[0];
    if (path_exists(path)) return printf_str("File already exists: %s", path);
    make_parent_dirs(path);
    FILE *f = c2a_fopen(path, "wb");
    if (!f) return printf_str("Error creating file: %s", strerror(errno));
    fclose(f);
    return printf_str("Successfully created file: %s", path);
}

static char *act_delete(char **p, int n, int want_dir) {
    if (n < 1) return xstrdup("Error: missing delete path");
    const char *path = p[0];
    if (!path_exists(path)) return printf_str("Path does not exist: %s", path);
    if (is_dir(path) != want_dir)
        return printf_str("Error: not a %s: %s", want_dir ? "directory" : "file", path);
    if (want_dir) {
        if (rmtree(path) != 0) return printf_str("Error deleting: %s", strerror(errno));
    } else {
        if (c2a_remove(path) != 0) return printf_str("Error deleting: %s", strerror(errno));
    }
    return printf_str("Successfully deleted: %s", path);
}

static char *act_rename(char **p, int n) {
    if (n < 2) return xstrdup("Error: missing rename params");
    const char *path = p[0];
    const char *new_name = p[1];
    if (!new_name[0] || strchr(new_name, '/') || strchr(new_name, '\\'))
        return xstrdup("Error: new_name must be a bare name without path separators");
    if (!path_exists(path)) return printf_str("Path does not exist: %s", path);
    char newpath[PROTO_MAX_PATH];
    path_with_name(path, new_name, newpath, sizeof newpath);
    if (path_exists(newpath)) return printf_str("Target name already exists: %s", new_name);
    if (c2a_rename(path, newpath) != 0) return printf_str("Error renaming: %s", strerror(errno));
    return printf_str("Successfully renamed: %s -> %s", path, new_name);
}

/* utf8_char_prefix returns the byte length of the longest prefix of s that is
 * at most max_chars UTF-8 code points and does not split a sequence. */
static size_t utf8_char_prefix(const char *s, size_t len, size_t max_chars) {
    size_t i = 0, chars = 0;
    while (i < len && chars < max_chars) {
        i++;
        while (i < len && ((unsigned char)s[i] & 0xC0) == 0x80) i++;
        chars++;
    }
    return i;
}

static char *act_read_file(char **p, int n) {
    if (n < 1) return xstrdup("Error: missing read_file path");
    const char *path = p[0];
    const char *sl = (n > 1) ? p[1] : "0";
    const char *el = (n > 2) ? p[2] : "0";
    if (!path_exists(path)) return printf_str("File does not exist: %s", path);
    if (is_dir(path)) return printf_str("%s is a directory", path);

    int whole = (sl == NULL || sl[0] == 0 || strcmp(sl, "0") == 0);
    if (whole) {
        /* Read a bounded prefix (enough for READ_FILE_LIMIT UTF-8 chars) so an
         * enormous file cannot exhaust memory, then truncate to the char limit
         * (4 bytes per char worst case + slack). */
        FILE *f = c2a_fopen(path, "rb");
        if (!f) return printf_str("Error reading file: %s", strerror(errno));
        size_t cap = (size_t)READ_FILE_LIMIT * 4 + 4;
        char *raw = (char *)malloc(cap + 1);
        if (!raw) { fclose(f); return xstrdup("Error reading file: out of memory"); }
        size_t got = fread(raw, 1, cap, f);
        fclose(f);
        size_t take = utf8_char_prefix(raw, got, READ_FILE_LIMIT);
        char *res = (char *)malloc(take + 1);
        if (!res) { free(raw); return xstrdup("Error reading file: out of memory"); }
        memcpy(res, raw, take);
        res[take] = 0;
        free(raw);
        return res;
    }

    /* line mode: stream the file, keeping only the requested range. */
    int start = 1;
    if (sl && sl[0] && strcmp(sl, "0") != 0) {
        char *sp = NULL;
        long sv = strtol(sl, &sp, 10);
        if (sp == sl || *sp != '\0' || sv < 1) return printf_str("Invalid start_line: %s", sl);
        start = (int)sv;
    }
    int end = 0; /* 0 = to end-of-file; invalid end_line falls back to it too */
    if (el && el[0] && strcmp(el, "0") != 0) {
        char *endp = NULL;
        long v = strtol(el, &endp, 10);
        if (endp != el && *endp == '\0' && v > 0) end = (int)v;
    }

    FILE *f = c2a_fopen(path, "rb");
    if (!f) return printf_str("Error reading file: %s", strerror(errno));
    strbuf_t out, cur;
    sb_init(&out);
    sb_init(&cur);
    char buf[65536];
    int lineNo = 0;
    int reachedStart = 0;
    int done = 0;
    for (;;) {
        size_t r = fread(buf, 1, sizeof buf, f);
        if (r == 0) break;
        for (size_t k = 0; k < r; k++) {
            char ch = buf[k];
            if (ch == '\n') {
                lineNo++;
                if (lineNo >= start) {
                    reachedStart = 1;
                    if (end > 0 && lineNo > end) { done = 1; break; }
                    if (cur.len) sb_append(&out, cur.data, cur.len);
                    sb_append(&out, "\n", 1);
                }
                cur.len = 0;
                if (cur.data) cur.data[0] = 0;
            } else if (sb_append(&cur, &ch, 1) != 0) {
                done = 1;
                break;
            }
        }
        if (done) break;
    }
    /* final line without a trailing newline */
    if (!done && cur.len > 0) {
        lineNo++;
        if (lineNo >= start && (end == 0 || lineNo <= end)) {
            reachedStart = 1;
            sb_append(&out, cur.data, cur.len);
        }
    }
    fclose(f);
    sb_free(&cur);
    if (!reachedStart) {
        sb_free(&out);
        return printf_str("Start line %s exceeds file line count (%d)", sl, lineNo);
    }
    return sb_take(&out);
}

static char *act_write_file(char **p, int n) {
    if (n < 2) return xstrdup("Error: missing write_file params");
    const char *path = p[0];
    const char *content = p[1];
    make_parent_dirs(path);
    FILE *f = c2a_fopen(path, "wb");
    if (!f) return printf_str("Error writing file: %s", strerror(errno));
    size_t clen = strlen(content);
    if (clen) fwrite(content, 1, clen, f);
    fclose(f);
    return printf_str("Successfully wrote to: %s", path);
}

/* Copy s into a fresh buffer with CRLF collapsed to LF. */
static char *normalize_lf(const char *s, size_t len, size_t *out_len) {
    char *out = (char *)malloc(len + 1);
    if (!out) return NULL;
    size_t o = 0;
    for (size_t i = 0; i < len; i++) {
        if (s[i] == '\r' && i + 1 < len && s[i + 1] == '\n') continue;
        out[o++] = s[i];
    }
    out[o] = 0;
    *out_len = o;
    return out;
}

/* Replace the single occurrence of old_text with new_text, mirroring the Go /
 * Python agents and Zed's edit_file: fail closed when old_text is missing or
 * not unique, so the model re-reads or adds context instead of editing the
 * wrong location. CRLF/LF differences are tolerated and the file's original
 * newline style is restored on write. */
static char *act_edit_file(char **p, int n) {
    if (n < 2) return xstrdup("Error: missing edit_file params");
    const char *path = p[0];
    const char *old_text = p[1];
    const char *new_text = (n > 2) ? p[2] : "";

    if (!path_exists(path)) return printf_str("File does not exist: %s", path);
    if (is_dir(path)) return printf_str("%s is a directory", path);
    if (old_text[0] == 0) return xstrdup("Error: old_text must not be empty");

    size_t blen = 0;
    char *raw = slurp(path, &blen);
    if (!raw) return printf_str("Error editing file: %s", strerror(errno));

    int had_crlf = 0;
    for (size_t k = 0; k + 1 < blen; k++) {
        if (raw[k] == '\r' && raw[k + 1] == '\n') { had_crlf = 1; break; }
    }

    size_t wlen = 0, ndlen = 0, rl = 0;
    char *work = normalize_lf(raw, blen, &wlen);
    free(raw);
    if (!work) return xstrdup("Error editing file: out of memory");
    char *needle = normalize_lf(old_text, strlen(old_text), &ndlen);
    if (!needle) { free(work); return xstrdup("Error editing file: out of memory"); }
    char *repl = normalize_lf(new_text, strlen(new_text), &rl);
    if (!repl) { free(work); free(needle); return xstrdup("Error editing file: out of memory"); }
    if (ndlen == 0) {
        free(work); free(needle); free(repl);
        return xstrdup("Error: old_text must not be empty");
    }

    size_t first = 0, count = 0;
    /* Advance by one byte so overlapping occurrences are counted too. */
    for (size_t pos = 0; pos + ndlen <= wlen; pos++) {
        if (memcmp(work + pos, needle, ndlen) == 0) {
            if (count == 0) first = pos;
            count++;
        }
    }
    if (count == 0) {
        free(work); free(needle); free(repl);
        return xstrdup("Error: old_text not found in file; read the file again to get the exact current content.");
    }
    if (count > 1) {
        char *e = printf_str("Error: old_text matched %d locations; include more surrounding context to make it unique.", (int)count);
        free(work); free(needle); free(repl);
        return e;
    }

    size_t outlen = wlen - ndlen + rl;
    char *out = (char *)malloc(outlen + 1);
    if (!out) { free(work); free(needle); free(repl); return xstrdup("Error editing file: out of memory"); }
    memcpy(out, work, first);
    memcpy(out + first, repl, rl);
    memcpy(out + first + rl, work + first + ndlen, wlen - first - ndlen);
    out[outlen] = 0;
    free(work); free(needle); free(repl);

    FILE *f = c2a_fopen(path, "wb");
    if (!f) {
        char *e2 = printf_str("Error editing file: %s", strerror(errno));
        free(out);
        return e2;
    }
    if (had_crlf) {
        for (size_t k = 0; k < outlen; k++) {
            if (out[k] == '\n') { fputc('\r', f); fputc('\n', f); }
            else fputc((unsigned char)out[k], f);
        }
    } else if (outlen) {
        fwrite(out, 1, outlen, f);
    }
    fclose(f);
    free(out);
    return printf_str("Successfully edited file: %s", path);
}

static char *act_copy(char **p, int n) {
    if (n < 2) return xstrdup("Error: missing copy params");
    const char *src = p[0];
    const char *dest = p[1];
    if (!path_exists(src)) return printf_str("Source not found: %s", src);
    if (is_dir(src)) {
        if (copy_tree(src, dest) != 0) return printf_str("Error copying: %s", strerror(errno));
    } else {
        make_parent_dirs(dest);
        if (copy_file(src, dest) != 0) return printf_str("Error copying: %s", strerror(errno));
    }
    return printf_str("Successfully copied: %s -> %s", src, dest);
}

static char *act_move(char **p, int n) {
    if (n < 2) return xstrdup("Error: missing move params");
    const char *src = p[0];
    const char *dest = p[1];
    if (!path_exists(src)) return printf_str("Source not found: %s", src);
    make_parent_dirs(dest);
    if (c2a_rename(src, dest) == 0)
        return printf_str("Successfully moved: %s -> %s", src, dest);
    /* Cross-device fallback: copy then delete. */
    if (is_dir(src)) {
        if (copy_tree(src, dest) != 0) return printf_str("Error moving: %s", strerror(errno));
        if (rmtree(src) != 0) return printf_str("Error moving: %s", strerror(errno));
    } else {
        if (copy_file(src, dest) != 0) return printf_str("Error moving: %s", strerror(errno));
        if (c2a_remove(src) != 0) return printf_str("Error moving: %s", strerror(errno));
    }
    return printf_str("Successfully moved: %s -> %s", src, dest);
}

/* ===================================================================== */
/* dispatcher                                                            */
/* ===================================================================== */

char *run_action(uint8_t cmd, char **p, int n, int cmd_timeout) {
    switch (cmd) {
        case CMD_GET_CWD:       return act_get_cwd();
        case CMD_LIST_DIR:      if (n < 1) return xstrdup("Error: missing list_dir path");
                                return act_list_dir(p[0]);
        case CMD_MAKE_DIR:      return act_make_dir(p, n);
        case CMD_CREATE_FILE:   return act_create_file(p, n);
        case CMD_DELETE_DIR:    return act_delete(p, n, 1);
        case CMD_DELETE_FILE:   return act_delete(p, n, 0);
        case CMD_RENAME_DIR:    return act_rename(p, n);
        case CMD_RENAME_FILE:   return act_rename(p, n);
        case CMD_READ_FILE:     return act_read_file(p, n);
        case CMD_WRITE_FILE:    return act_write_file(p, n);
        case CMD_EDIT_FILE:     return act_edit_file(p, n);
        case CMD_COPY:          return act_copy(p, n);
        case CMD_MOVE:          return act_move(p, n);
        case CMD_EXEC_CMD:      if (n < 1) return xstrdup("Error: missing exec_cmd command");
                                return run_cmd(p[0], cmd_timeout);
        default:                return printf_str("Error: Unknown action: %d", cmd);
    }
}
