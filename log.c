#include <stdio.h>
#include <string.h>
#include <errno.h>
#include "log.h"

void lerror(char *msg) {
    fprintf(stderr, "tty: %s: %s\n", msg, strerror(errno));
}

void lmsg(char *msg) {
    fprintf(stderr, "tty: %s\n", msg);
}