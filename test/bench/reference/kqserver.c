/* kqserver.c - a kqueue event-loop RESP server that stores nothing.
 *
 * Not part of the build. It is the architectural control behind the P8
 * recommendation in dev/benchmarks/P2-network-baseline.txt, kept here so that
 * recommendation can be re-derived rather than taken on trust.
 *
 * ADR-0007 defers the gnet decision to benchmark evidence. The evidence needed
 * is not "how fast is AtlasCache" but "how fast could any server be on this
 * loopback, with an event loop instead of a goroutine per connection". That
 * needs an event loop to compare against. This is one: N threads, each with
 * SO_REUSEPORT, its own listener and its own kqueue, and no per-request thread
 * parking. It parses RESP requests and answers a canned bulk reply.
 *
 *   cc -O2 -o kqserver kqserver.c
 *   ./kqserver <port> <threads> <reply-bytes>
 *
 * Then point the Go generator at it:
 *
 *   go test ./test/bench -run TestNetworkProbe -netbench.probe \
 *       -netbench.addr 127.0.0.1:7500 -netbench.conns 50 -netbench.pipe 1 \
 *       -netbench.preload=false
 */
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <unistd.h>

#define BUFSZ (1024 * 1024)

static char g_reply[128];
static size_t g_reply_len;
static int g_port, g_threads;

typedef struct {
    int fd;
    unsigned char *in;
    size_t inlen;
    unsigned char *out;
    size_t outcap;
} conn_t;

/* Consume as many complete RESP array requests as the buffer holds.
   Returns bytes consumed and sets *ncmds; -1 on a malformed request. */
static long scan_requests(const unsigned char *buf, size_t len, int *ncmds) {
    size_t pos = 0;
    int n = 0;
    for (;;) {
        size_t p = pos;
        if (p >= len) break;
        if (buf[p] != '*') return -1;
        const unsigned char *nl = memchr(buf + p, '\n', len - p);
        if (!nl) break;
        int nargs = atoi((const char *)buf + p + 1);
        if (nargs < 0) return -1;
        p = (size_t)(nl - buf) + 1;
        int ok = 1;
        for (int i = 0; i < nargs; i++) {
            if (p >= len || buf[p] != '$') { ok = 0; break; }
            const unsigned char *nl2 = memchr(buf + p, '\n', len - p);
            if (!nl2) { ok = 0; break; }
            long sz = atol((const char *)buf + p + 1);
            if (sz < 0) return -1;
            p = (size_t)(nl2 - buf) + 1;
            if (p + (size_t)sz + 2 > len) { ok = 0; break; }
            p += (size_t)sz + 2;
        }
        if (!ok) break;
        pos = p;
        n++;
    }
    *ncmds = n;
    return (long)pos;
}

static void write_all(int fd, const unsigned char *buf, size_t len) {
    size_t off = 0;
    while (off < len) {
        ssize_t w = write(fd, buf + off, len - off);
        if (w > 0) { off += (size_t)w; continue; }
        if (w < 0 && (errno == EAGAIN || errno == EINTR)) { usleep(50); continue; }
        return;
    }
}

static void close_conn(int kq, conn_t *c) {
    struct kevent ev;
    EV_SET(&ev, c->fd, EVFILT_READ, EV_DELETE, 0, 0, NULL);
    kevent(kq, &ev, 1, NULL, 0, NULL);
    close(c->fd);
    free(c->in);
    free(c->out);
    free(c);
}

static void on_readable(int kq, conn_t *c) {
    ssize_t r = read(c->fd, c->in + c->inlen, BUFSZ - c->inlen);
    if (r <= 0) {
        if (r < 0 && (errno == EAGAIN || errno == EINTR)) return;
        close_conn(kq, c);
        return;
    }
    c->inlen += (size_t)r;

    int ncmds = 0;
    long used = scan_requests(c->in, c->inlen, &ncmds);
    if (used < 0) { close_conn(kq, c); return; }
    if (used > 0) {
        memmove(c->in, c->in + used, c->inlen - (size_t)used);
        c->inlen -= (size_t)used;
    }
    if (ncmds == 0) return;

    size_t need = (size_t)ncmds * g_reply_len;
    if (need > c->outcap) {
        c->out = realloc(c->out, need);
        c->outcap = need;
    }
    for (int i = 0; i < ncmds; i++)
        memcpy(c->out + (size_t)i * g_reply_len, g_reply, g_reply_len);
    write_all(c->fd, c->out, need);
}

static void *loop(void *arg) {
    (void)arg;
    int ln = socket(AF_INET, SOCK_STREAM, 0);
    int one = 1;
    setsockopt(ln, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    setsockopt(ln, SOL_SOCKET, SO_REUSEPORT, &one, sizeof(one));
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = htons((uint16_t)g_port);
    if (bind(ln, (struct sockaddr *)&addr, sizeof(addr)) != 0) { perror("bind"); exit(1); }
    if (listen(ln, 1024) != 0) { perror("listen"); exit(1); }
    fcntl(ln, F_SETFL, O_NONBLOCK); /* the accept loop drains until EAGAIN */

    int kq = kqueue();
    struct kevent ev;
    EV_SET(&ev, ln, EVFILT_READ, EV_ADD, 0, 0, NULL);
    kevent(kq, &ev, 1, NULL, 0, NULL);

    struct kevent events[256];
    for (;;) {
        int n = kevent(kq, NULL, 0, events, 256, NULL);
        for (int i = 0; i < n; i++) {
            if ((int)events[i].ident == ln) {
                for (;;) {
                    int fd = accept(ln, NULL, NULL);
                    if (fd < 0) break;
                    fcntl(fd, F_SETFL, O_NONBLOCK);
                    setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
                    conn_t *c = calloc(1, sizeof(conn_t));
                    c->fd = fd;
                    c->in = malloc(BUFSZ);
                    c->outcap = 64 * 1024;
                    c->out = malloc(c->outcap);
                    EV_SET(&ev, fd, EVFILT_READ, EV_ADD, 0, 0, c);
                    kevent(kq, &ev, 1, NULL, 0, NULL);
                }
                continue;
            }
            conn_t *c = (conn_t *)events[i].udata;
            if (!c) continue;
            if (events[i].flags & EV_EOF && events[i].data == 0) { close_conn(kq, c); continue; }
            on_readable(kq, c);
        }
    }
    return NULL;
}

int main(int argc, char **argv) {
    if (argc < 4) { fprintf(stderr, "usage: %s <port> <threads> <reply-bytes>\n", argv[0]); return 2; }
    g_port = atoi(argv[1]);
    g_threads = atoi(argv[2]);
    int size = atoi(argv[3]);
    int hdr = snprintf(g_reply, sizeof(g_reply), "$%d\r\n", size);
    memset(g_reply + hdr, 'x', (size_t)size);
    g_reply[hdr + size] = '\r';
    g_reply[hdr + size + 1] = '\n';
    g_reply_len = (size_t)hdr + (size_t)size + 2;

    pthread_t th[64];
    for (int i = 1; i < g_threads; i++) pthread_create(&th[i], NULL, loop, NULL);
    loop(NULL);
    return 0;
}
