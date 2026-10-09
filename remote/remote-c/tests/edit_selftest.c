/* Self-test for the C agent's exact-string edit_file semantics.
 *
 * Build (from remote/remote-c):
 *   Windows: gcc -O2 -Wall -Wextra -o edit_selftest tests/edit_selftest.c protocol.c -lws2_32
 *   POSIX:   gcc -O2 -Wall -Wextra -o edit_selftest tests/edit_selftest.c protocol.c -lpthread
 */
#include <stdio.h>
#include <string.h>

#include "../actions.c"

/* The edit path never calls run_cmd; stub it so linking the unused action
 * dispatcher succeeds. */
char *run_cmd(const char *cmd, int timeout_sec) {
    (void)cmd;
    (void)timeout_sec;
    return NULL;
}

static int put_file(const char *path, const char *data) {
    FILE *f = fopen(path, "wb");
    if (!f) return -1;
    fwrite(data, 1, strlen(data), f);
    fclose(f);
    return 0;
}

static int get_file(const char *path, char *buf, size_t cap) {
    FILE *f = fopen(path, "rb");
    if (!f) return -1;
    size_t n = fread(buf, 1, cap - 1, f);
    fclose(f);
    buf[n] = 0;
    return (int)n;
}

int main(void) {
    const char *p = "edit_selftest.tmp";
    char buf[512];
    char *r;

    /* unique multi-line replace on a CRLF file must preserve CRLF */
    put_file(p, "a\r\nb\r\nc\r\n");
    char *e1[] = { (char *)p, (char *)"b\n", (char *)"x\ny\n" };
    r = act_edit_file(e1, 3);
    if (!r || strncmp(r, "Successfully", 12) != 0) {
        fprintf(stderr, "replace failed: %s\n", r ? r : "(null)");
        return 1;
    }
    get_file(p, buf, sizeof buf);
    if (strcmp(buf, "a\r\nx\r\ny\r\nc\r\n") != 0) {
        fprintf(stderr, "CRLF/body mismatch: %s\n", buf);
        return 1;
    }

    /* duplicate old_text must be rejected */
    put_file(p, "x\nx\n");
    char *e2[] = { (char *)p, (char *)"x\n", (char *)"y\n" };
    r = act_edit_file(e2, 3);
    if (!r || strncmp(r, "Error:", 6) != 0) {
        fprintf(stderr, "ambiguous not rejected: %s\n", r ? r : "(null)");
        return 1;
    }

    /* missing old_text must be rejected */
    char *e3[] = { (char *)p, (char *)"zzz", (char *)"y" };
    r = act_edit_file(e3, 3);
    if (!r || strncmp(r, "Error:", 6) != 0) {
        fprintf(stderr, "not-found not rejected\n");
        return 1;
    }

    /* empty old_text must be rejected */
    char *e4[] = { (char *)p, (char *)"", (char *)"y" };
    r = act_edit_file(e4, 3);
    if (!r || strncmp(r, "Error:", 6) != 0) {
        fprintf(stderr, "empty old_text not rejected\n");
        return 1;
    }

    /* overlapping duplicate old_text must be rejected */
    put_file(p, "aaa");
    char *e5[] = { (char *)p, (char *)"aa", (char *)"b" };
    r = act_edit_file(e5, 3);
    if (!r || strncmp(r, "Error:", 6) != 0) {
        fprintf(stderr, "overlapping not rejected: %s\n", r ? r : "(null)");
        return 1;
    }

    remove(p);
    printf("edit_selftest: OK\n");
    return 0;
}
