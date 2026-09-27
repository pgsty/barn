#include "Bridge.h"
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/stat.h>
#include <sys/file.h>
#include <unistd.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <stdint.h>
#include <fcntl.h>
#include <arpa/inet.h>

static int fail(char *buf, size_t len, const char *message) {
    snprintf(buf, len, "%s: %s", message, strerror(errno));
    return -1;
}
static int address(const char *path, struct sockaddr_un *addr) {
    memset(addr, 0, sizeof(*addr));
    if (strlen(path) >= sizeof(addr->sun_path)) { errno = ENAMETOOLONG; return -1; }
    addr->sun_family = AF_UNIX;
    strlcpy(addr->sun_path, path, sizeof(addr->sun_path));
    addr->sun_len = (uint8_t)sizeof(*addr);
    return 0;
}
static void limits(int fd) {
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof(one));
    struct timeval timeout = { 15, 0 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    fcntl(fd, F_SETFD, FD_CLOEXEC);
}
int fm_unix_listen(const char *path, char *error, size_t len) {
    struct sockaddr_un addr;
    if (address(path, &addr) < 0) return fail(error, len, "RPC path");
    struct stat st;
    if (lstat(path, &st) == 0) {
        if (!S_ISSOCK(st.st_mode) || st.st_uid != getuid()) { errno = EPERM; return fail(error, len, "unsafe RPC path"); }
        /* An existing listener must never be replaced. */
        int existing = fm_unix_connect(path, error, len);
        if (existing >= 0) { close(existing); errno = EADDRINUSE; return fail(error, len, "RPC already running"); }
        if (unlink(path) < 0) return fail(error, len, "remove stale RPC socket");
    }
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return fail(error, len, "RPC socket");
    limits(fd);
    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0 || chmod(path, 0600) < 0 || listen(fd, 8) < 0) {
        int saved = errno; close(fd); errno = saved; return fail(error, len, "RPC listen");
    }
    return fd;
}
int fm_unix_accept(int server) {
    int fd = accept(server, NULL, NULL);
    if (fd < 0) return -1;
    uid_t uid; gid_t gid;
    if (getpeereid(fd, &uid, &gid) < 0 || uid != getuid()) { close(fd); errno = EACCES; return -1; }
    limits(fd);
    return fd;
}
int fm_unix_connect(const char *path, char *error, size_t len) {
    struct sockaddr_un addr;
    if (address(path, &addr) < 0) return fail(error, len, "socket path");
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return fail(error, len, "control socket");
    limits(fd);
    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        int saved = errno; close(fd); errno = saved; return fail(error, len, "connect socket");
    }
    uid_t uid; gid_t gid;
    if (getpeereid(fd, &uid, &gid) < 0 || (uid != getuid() && uid != 0)) {
        close(fd); errno = EACCES; return fail(error, len, "socket peer identity");
    }
    return fd;
}
int fm_lock(const char *path) {
    int fd = open(path, O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0600);
    if (fd < 0) return -1;
    if (flock(fd, LOCK_EX | LOCK_NB) < 0) { close(fd); return -1; }
    return fd;
}
