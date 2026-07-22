#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <pthread.h>
#include <unistd.h>
#include <netdb.h>
#include <sys/un.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <termios.h>
#include <fcntl.h>
#include <libgen.h>

#include "systemd.h"
#include "log.h"

int op_resize = 1;
int sock_fd;
int tty_fd;

struct copy_data
{
    int w;
    int r;
};

void *copy(void *args)
{
    struct copy_data *cp;

    cp = (struct copy_data *)args;

    char buf[1024];
    int n;

    while ((n = read(cp->r, buf, sizeof(buf))) > 0)
        n = write(cp->w, buf, n);

    return 0;
}

// write_all writes the whole buffer, looping over short writes.
// Returns 0 on success and -1 if any write fails.
int write_all(int fd, const char *buf, size_t len)
{
    size_t off = 0;
    while (off < len)
    {
        ssize_t n = write(fd, buf + off, len - off);
        if (n < 0)
            return -1;
        off += (size_t)n;
    }
    return 0;
}

// The tty client protocol is a newline-framed status line per request:
//   "0\n"            on success
//   "1 <message>\n"  on error
// reply_ok/reply_err write one such line. They return 0 on success and -1 on
// write failure.
int reply_ok(int fd)
{
    return write_all(fd, "0\n", 2);
}

int reply_err(int fd, const char *msg)
{
    char buf[256];
    int n = snprintf(buf, sizeof(buf), "1 %s\n", msg);
    if (n < 0)
        return -1;
    if (n >= (int)sizeof(buf))
        n = sizeof(buf) - 1;
    return write_all(fd, buf, (size_t)n);
}

// handle_tty_request services a single request line (newline already stripped),
// writing exactly one status line back to the client. It returns 0 when the
// connection should stay open -- including recoverable errors that were reported
// to the client -- and -1 when a socket write failed and the caller should close.
int handle_tty_request(int fd, const char *line)
{
    int op, w, h;
    if (sscanf(line, "%d %d %d", &op, &w, &h) != 3)
    {
        lerror("parse");
        return reply_err(fd, "invalid resize request");
    }

    if (op != op_resize)
        return reply_err(fd, "unknown operation");

    struct winsize ws;
    memset(&ws, 0, sizeof(ws));
    ws.ws_col = w;
    ws.ws_row = h;

    if (ioctl(tty_fd, TIOCSWINSZ, &ws) < 0)
    {
        lerror("ioctl TIOCSWINSZ");
        return reply_err(fd, "error setting window size");
    }

    return reply_ok(fd);
}

void handle_tty_op_conn(int fd)
{
    char buf[256];
    // len is how many bytes of a not-yet-complete request are buffered. The
    // socket is a byte stream, so a single read() may return a partial request
    // or several requests at once; we buffer until each '\n' before parsing so
    // fragmentation cannot desynchronize requests from responses.
    size_t len = 0;

    while (1)
    {
        // Leave room to NUL-terminate for sscanf.
        ssize_t nr = read(fd, buf + len, sizeof(buf) - 1 - len);
        if (nr < 0)
        {
            lerror("read");
            close(fd);
            return;
        }
        if (nr == 0)
        {
            // The client closed the connection.
            close(fd);
            return;
        }
        len += (size_t)nr;

        // Service every complete, newline-terminated request in the buffer.
        char *start = buf;
        char *nl;
        while ((nl = memchr(start, '\n', (buf + len) - start)) != NULL)
        {
            *nl = '\0';
            if (handle_tty_request(fd, start) < 0)
            {
                lerror("write");
                close(fd);
                return;
            }
            start = nl + 1;
        }

        // Keep any trailing partial request for the next read.
        len -= (size_t)(start - buf);
        memmove(buf, start, len);

        // A request that fills the buffer without a newline is malformed.
        if (len == sizeof(buf) - 1)
        {
            reply_err(fd, "request too long");
            close(fd);
            return;
        }
    }
}

void *handle_tty_ops(void *args)
{
    // This fd came from go, so you know it is in non-blocking mode.
    // We aren't doing non-blocking I/O here so we need to set it back to blocking.
    int fd_flags = fcntl(sock_fd, F_GETFL);
    if (fd_flags < 0)
    {
        lerror("F_GETFL");
        return 0;
    }

    int err = fcntl(sock_fd, F_SETFL, fd_flags & ~O_NONBLOCK);
    if (err < 0)
    {
        lerror("F_SETFL");
        return 0;
    }

    lmsg("Ready for tty clients\n");

    while (1)
    {
        int cfd = accept(sock_fd, NULL, NULL);
        if (cfd < 0)
        {
            lerror("accept");
            close(sock_fd);
            return 0;
        }

        // Only handle one connection at a time.
        // This should only have one client.
        handle_tty_op_conn(cfd);
    }
}

void setcgroup(void)
{
    char *val = getenv("SHIM_CGROUP");
    if (val == NULL)
        return;

    char *CG_PATH = "/sys/fs/cgroup";
    char *CG_PROCS = "cgroup.procs";

    int sz = strlen(CG_PATH) + strlen(val) + 1 + strlen(CG_PROCS);
    char p[sz];

    sprintf(p, "%s/%s/%s", CG_PATH, val, CG_PROCS);
    lmsg("Setting cgroup");

    FILE *fd = fopen(p, "w");
    if (fd < 0)
    {
        lerror("fopen");
        return;
    }

    lmsg("opened cgroup.procs");

    pid_t pid = getpid();
    int err = fprintf(fd, "%d\n", pid);
    if (err < 0)
    {
        lerror("write cgroup.procs");
    }
    lmsg("wrote cgroup.procs");
    err = fclose(fd);
    if (err < 0)
    {
        lerror("fclose");
    }
}

int handle_pty(void)
{
    pthread_t stdin_copy_thr_id, stdout_copy_thr_id, tty_op_thr_id;
    struct copy_data *stdin_copy = (struct copy_data *)malloc(sizeof(struct copy_data));
    stdin_copy->w = tty_fd;
    stdin_copy->r = 0;
    pthread_create(&stdin_copy_thr_id, NULL, copy, (void *)stdin_copy);

    struct copy_data *stdout_copy = (struct copy_data *)malloc(sizeof(struct copy_data));
    stdout_copy->w = 1;
    stdout_copy->r = tty_fd;
    pthread_create(&stdout_copy_thr_id, NULL, copy, (void *)stdout_copy);

    pthread_create(&tty_op_thr_id, NULL, handle_tty_ops, (void *)NULL);

    pthread_join(stdout_copy_thr_id, NULL);
    free(stdout_copy);

    pthread_cancel(stdin_copy_thr_id);
    pthread_join(stdin_copy_thr_id, NULL);
    free(stdin_copy);

    close(tty_fd);
    close(sock_fd);

    lmsg("Exiting");
    exit(0);
}

int mkdir_all(char *path)
{
    char *p = path;
    while (*p != '\0')
    {
        if (*p == '/')
        {
            *p = '\0';
            mkdir(path, 0755);
            *p = '/';
        }
        p++;
    }
    return 0;
}

int tty_recv_fd(char *sock_path)
{
    sock_fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (sock_fd < 0)
    {
        return sock_fd;
    }
    lmsg("created socket");

    unlink(sock_path);

    char *dir = strdup(sock_path);
    int err = mkdir_all(dirname(dir));
    free(dir);
    if (err < 0)
    {
        close(sock_fd);
        return err;
    }

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(struct sockaddr_un));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, sock_path, sizeof(addr.sun_path) - 1);

    lmsg("binding tty socket path");
    lmsg(sock_path);

    err = bind(sock_fd, (struct sockaddr *)&addr, sizeof(struct sockaddr_un));
    if (err < 0)
    {
        close(sock_fd);
        return err;
    }

    lmsg("bound socket");

    err = listen(sock_fd, 1);
    if (err < 0)
    {
        close(sock_fd);
        return err;
    }

    lmsg("listening on socket");

    err = sd_notify("READY=1");
    if (err < 0)
    {
        lerror("sd_notify");
    }
    else
    {
        lmsg("sd_notify READY=1");
    }

    int conn = accept(sock_fd, NULL, NULL);
    if (conn < 0)
    {
        close(sock_fd);
        return conn;
    }

    lmsg("accepted connection");

    char buf[512];
    struct iovec e = {buf, 512};
    char cmsg[CMSG_SPACE(sizeof(int))];
    struct msghdr m = {NULL, 0, &e, 1, cmsg, sizeof(cmsg), 0};

    int n = recvmsg(conn, &m, 0);
    if (n < 0)
    {
        close(conn);
        close(sock_fd);
        return n;
    }

    lmsg("received fd");

    struct cmsghdr *c = CMSG_FIRSTHDR(&m);
    int fd = *(int *)CMSG_DATA(c);

    close(conn);
    // We re-use sock_fd for the tty operations, so don't close that.
    return fd;
}

void pty_main(void)
{
    char *val = getenv("_TTY_HANDSHAKE");
    if (val == NULL || *val != '1')
    {
        return;
    }

    char *sock_path = getenv("_TTY_SOCKET_PATH");
    if (sock_path == NULL)
    {
        lerror("no socket path");
        exit(1);
    }

    setcgroup();
    lmsg("cgroup set");

    tty_fd = tty_recv_fd(sock_path);
    if (tty_fd < 0)
    {
        lerror("tty_recv_fd");
        exit(2);
    }

    // TODO: make this configurable
    // Maybe we can use the container uid by default (unless it is root)?
    if (setuid(10000) < 0)
    {
        lerror("setuid");
        exit(1);
    }

    if (handle_pty() < 0)
    {
        lerror("handle_pty");
        exit(3);
    }
}
