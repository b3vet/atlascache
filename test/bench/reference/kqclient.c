/* kqclient.c - a kqueue event-loop RESP load client.
 *
 * Not part of the build. It is the independent generator behind the
 * "the load generator is not the bottleneck" section of
 * dev/benchmarks/P2-network-baseline.txt.
 *
 * The Go generator in this directory is a goroutine per connection, which is
 * the same shape as the server it measures; if that shape has a ceiling, the
 * generator and the server hit it together and the sweep would report the
 * ceiling as the server's limit. This client shares nothing with it -- one
 * thread, one kqueue, no goroutines, about one core -- so agreement between the
 * two is evidence about the server rather than about Go.
 *
 * Closed loop: every connection keeps one GET in flight. Replies are a fixed
 * 71 bytes ($64\r\n + 64 + \r\n), so completion is counted in bytes -- which
 * means the keyspace must already hold 64-byte values under the keys below, and
 * a miss would desynchronize the count.
 *
 *   cc -O2 -o kqclient kqclient.c
 *   ./kqclient <port> <conns> <seconds>
 */
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#define REPLY_LEN 71
#define KEYS 100000

typedef struct {
    int fd;
    size_t pending;   /* reply bytes still to arrive */
    long key;
    char req[64];
    int reqlen;
} conn_t;

static double now_sec(void) {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return (double)tv.tv_sec + (double)tv.tv_usec / 1e6;
}

static void build_req(conn_t *c) {
    char key[32];
    int klen = snprintf(key, sizeof(key), "bench:key:%09ld", c->key % KEYS);
    c->reqlen = snprintf(c->req, sizeof(c->req), "*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", klen, key);
    c->key++;
}

static void send_req(conn_t *c) {
    build_req(c);
    ssize_t off = 0;
    while (off < c->reqlen) {
        ssize_t w = write(c->fd, c->req + off, (size_t)c->reqlen - (size_t)off);
        if (w > 0) { off += w; continue; }
        if (w < 0 && (errno == EAGAIN || errno == EINTR)) { usleep(20); continue; }
        return;
    }
    c->pending = REPLY_LEN;
}

int main(int argc, char **argv) {
    if (argc < 4) { fprintf(stderr, "usage: %s <port> <conns> <seconds>\n", argv[0]); return 2; }
    int port = atoi(argv[1]), nconn = atoi(argv[2]);
    double secs = atof(argv[3]);

    conn_t *conns = calloc((size_t)nconn, sizeof(conn_t));
    int kq = kqueue();
    struct kevent ev;

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = htons((uint16_t)port);

    for (int i = 0; i < nconn; i++) {
        int fd = socket(AF_INET, SOCK_STREAM, 0);
        if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) { perror("connect"); return 1; }
        int one = 1;
        setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
        fcntl(fd, F_SETFL, O_NONBLOCK);
        conns[i].fd = fd;
        conns[i].key = i * 977;
        EV_SET(&ev, fd, EVFILT_READ, EV_ADD, 0, 0, &conns[i]);
        kevent(kq, &ev, 1, NULL, 0, NULL);
    }

    /* Warm up for a second, then measure. */
    for (int i = 0; i < nconn; i++) send_req(&conns[i]);

    unsigned char buf[262144];
    struct kevent events[512];
    long ops = 0;
    double t_start = now_sec(), t_measure = t_start + 1.0, t_end = t_measure + secs;
    int measuring = 0;

    for (;;) {
        struct timespec ts = {0, 20 * 1000 * 1000};
        int n = kevent(kq, NULL, 0, events, 512, &ts);
        for (int i = 0; i < n; i++) {
            conn_t *c = (conn_t *)events[i].udata;
            ssize_t r = read(c->fd, buf, sizeof(buf));
            if (r <= 0) continue;
            size_t got = (size_t)r;
            while (got > 0) {
                size_t take = got < c->pending ? got : c->pending;
                c->pending -= take;
                got -= take;
                if (c->pending == 0) {
                    if (measuring) ops++;
                    send_req(c);
                    if (got > 0) continue;
                }
            }
        }
        double t = now_sec();
        if (!measuring && t >= t_measure) { measuring = 1; t_start = t; }
        if (measuring && t >= t_end) {
            printf("KQCLIENT port=%d conns=%d ops=%ld ops/s=%.0f mean_rtt=%.1fus\n",
                   port, nconn, ops, (double)ops / (t - t_start),
                   (t - t_start) * 1e6 * nconn / (double)ops);
            return 0;
        }
    }
}
