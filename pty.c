#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <pthread.h>
#include <unistd.h>
#include <netdb.h>
#include <sys/un.h>
#include <sys/ioctl.h>
#include <termios.h>
#include <fcntl.h>


int op_resize = 1;
int tty_fd = 100;
int sock_fd = 101;

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
            perror("read");
            close(fd);
            return;
        }

        if (nr < 1)
        {
            char *msg = "invalid operation";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                perror("write");
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
                perror("write");
                close(fd);
                return;
            }
            perror("parse");
            close(fd);
            return;
        }

        if (op != op_resize)
        {
            char *msg = "invalid operation";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                perror("write");
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
            perror("ioctl TIOCSWINSZ");
            char *msg = "error setting win size";
            nw = write(fd, msg, sizeof(msg));
            if (nw < 0)
            {
                perror("write");
                close(fd);
                return;
            }
        }

        // Send ack
        nw = write(fd, "0", 1);
        if (nw < 1) {
            perror("write");
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
        perror("F_GETFL");
        return 0;
    }

    int err = fcntl(sock_fd, F_SETFL, fd_flags & ~O_NONBLOCK);
    if (err < 0)
    {
        perror("F_SETFL");
        return 0;
    }

    fprintf(stderr, "Ready for tty clients\n");

    while (1)
    {
        int cfd = accept(sock_fd, NULL, NULL);
        if (cfd < 0)
        {
            perror("accept");
            close(sock_fd);
            return 0;
        }

        // Only handle one connection at a time.
        // This should only have one client.
        handle_tty_op_conn(cfd);
    }
}

void handle_pty(void)
{
    char *val;

    val = getenv("_TTY_HANDSHAKE");
    if (val == NULL || *val != '2')
    {
        return;
    }

    // TODO: make this configurable
    // Maybe we can use the container uid by default (unless it is root)?
    int err = setuid(10000);
    if (err < 0)
    {
        perror("setuid");
        return;
    }

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

    fprintf(stderr, "Exiting\n");
    exit(0);
}

