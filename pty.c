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

struct copy_data {
    int w;
    int r;
};

void *copy(void *args)
{
    struct copy_data *cp;

    cp = (struct copy_data*)args;

    char buf[1024];
    int n;

    while ((n = read(cp->r, buf, sizeof(buf))) > 0)
        n = write(cp->w, buf, n);

    return 0;
}


void handle_tty_op_conn(int fd)
{
    int nr, nw;
    char buf[256];

    while (1) {
        nr = read(fd, buf, sizeof(buf));
        if (nr < 0 )
        {
            lerror("read");
            close(fd);
            return;
        }

        if (nr < 1)
        {
            char *msg = "invalid operation";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                lerror("write");
                close(fd);
                return;
            }
        }

        int op, w, h;
        int err;

        char op_buf[nr];

        // TODO: this is pretty sloppy
        // We aren't checking that the data sent is valid and includes everything we expect.
        memcpy(op_buf, buf, nr);
        err = sscanf(op_buf, "%d %d %d", &op, &w, &h);
        if (err < 0)
        {
            char *msg = "parse error";
            nw = write(fd, sizeof(msg)+msg, sizeof(msg));
            if (nw < 0)
            {
                lerror("write");
                close(fd);
                return;
            }
            lerror("parse");
            close(fd);
            return;
        }

        if (op != op_resize)
        {
            char *msg = "invalid operation";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                lerror("write");
                close(fd);
                return;
            }
        }

        struct winsize* ws = (struct winsize*)malloc(sizeof(struct winsize));
        ws->ws_col = w;
        ws->ws_row = h;

        err = ioctl(tty_fd, TIOCSWINSZ, ws);
        free(ws);
        if (err < 0 )
        {
            lerror("ioctl TIOCSWINSZ");
            char *msg = "error setting win size";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                lerror("write");
                close(fd);
                return;
            }
        }

        // Send ack
        nw = write(fd, "0", 1);
        if (nw < 1) {
            lerror("write");
            close(fd);
            return;
        }
    }

    close(fd);
    return;
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

    pthread_join(stdin_copy_thr_id, NULL);
    free(stdin_copy);
    close(tty_fd);
    pthread_join(stdout_copy_thr_id, NULL);
    free(stdout_copy);

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
    } else {
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

    if (handle_pty() < 0) {
        lerror("handle_pty");
        exit(3);
    }
}
