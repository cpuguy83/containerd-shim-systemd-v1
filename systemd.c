#include <sys/types.h>
#include <sys/socket.h>
#include <stdlib.h>
#include <string.h>
#include <sys/un.h>
#include <unistd.h>
#include "systemd.h"

int sd_notify(const char *state) {
    char *e;
    e = getenv("NOTIFY_SOCKET");
    if (!e)
        return 0;
    if (e[0] == '@')
        return -1;


    int fd = socket(AF_UNIX, SOCK_DGRAM|SOCK_CLOEXEC, 0);
    if (fd < 0)
        return fd;


    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strcpy(addr.sun_path, e);

    int err = connect(fd, (struct sockaddr *)&addr, sizeof(addr));
    if (err < 0)
    {
        close(fd);
        return err;
    }

    return sendto(fd, state, strlen(state), 0, (struct sockaddr *)&addr, sizeof(addr));
}
