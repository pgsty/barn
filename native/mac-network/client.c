#include "client.h"
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

static int fail(int control, int data, char *error, size_t size, const char *message) {
    int saved = errno;
    if (control >= 0) close(control);
    if (data >= 0) close(data);
    if (error && size) snprintf(error, size, "%s", message);
    errno = saved;
    return -1;
}

int farrow_mac_network_connect(const char *path, int slot, int *control_fd,
                               char *error, size_t error_size) {
    if (control_fd) *control_fd = -1;
    struct sockaddr_un address = { .sun_family = AF_UNIX };
    if (!path || !control_fd || slot < 1 || slot > 2 ||
        strlen(path) >= sizeof(address.sun_path)) {
        errno = EINVAL;
        return fail(-1, -1, error, error_size, "Invalid Mac network socket path or slot");
    }
    strcpy(address.sun_path, path);
    address.sun_len = sizeof(address);
    int control = socket(AF_UNIX, SOCK_STREAM, 0);
    if (control < 0) return fail(-1, -1, error, error_size, "Cannot create network control socket");
    fcntl(control, F_SETFD, FD_CLOEXEC);
    int yes = 1;
    setsockopt(control, SOL_SOCKET, SO_NOSIGPIPE, &yes, sizeof(yes));
    struct timeval timeout = { .tv_sec = 15 };
    setsockopt(control, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(control, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    if (connect(control, (struct sockaddr *)&address, sizeof(address)) < 0) {
        char message[256];
        snprintf(message, sizeof(message), "Mac network helper is unavailable: %s; run farrow mac setup", strerror(errno));
        return fail(control, -1, error, error_size, message);
    }
    uid_t peer_uid;
    gid_t peer_gid;
    if (getpeereid(control, &peer_uid, &peer_gid) < 0 || peer_uid != 0) {
        errno = EACCES;
        return fail(control, -1, error, error_size, "Mac network helper peer is not root");
    }
    unsigned char request[8] = {'F', 'M', 'N', '1', (unsigned char)slot, 0, 0, 0};
    size_t sent = 0;
    while (sent < sizeof(request)) {
        ssize_t count = send(control, request + sent, sizeof(request) - sent, 0);
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) return fail(control, -1, error, error_size, "Network helper handshake write failed");
        sent += (size_t)count;
    }
    unsigned char response[8] = {0};
    union { struct cmsghdr alignment; unsigned char bytes[CMSG_SPACE(sizeof(int) * 8)]; } ancillary;
    struct iovec iov = { .iov_base = response, .iov_len = sizeof(response) };
    struct msghdr msg = { .msg_iov = &iov, .msg_iovlen = 1,
                         .msg_control = ancillary.bytes, .msg_controllen = sizeof(ancillary.bytes) };
    ssize_t count;
    do { count = recvmsg(control, &msg, 0); } while (count < 0 && errno == EINTR);
    if (count <= 0) return fail(control, -1, error, error_size, "Network helper handshake timed out or closed");
    int data = -1, fd_count = 0;
    for (struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg); cmsg; cmsg = CMSG_NXTHDR(&msg, cmsg)) {
        if (cmsg->cmsg_level != SOL_SOCKET || cmsg->cmsg_type != SCM_RIGHTS || cmsg->cmsg_len < CMSG_LEN(0)) continue;
        size_t n = (cmsg->cmsg_len - CMSG_LEN(0)) / sizeof(int);
        int *fds = (int *)CMSG_DATA(cmsg);
        for (size_t i = 0; i < n; ++i) {
            if (fd_count++ == 0) data = fds[i]; else close(fds[i]);
        }
    }
    size_t received = (size_t)count;
    while (received < sizeof(response)) {
        count = recv(control, response + received, sizeof(response) - received, 0);
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) return fail(control, data, error, error_size, "Truncated network helper response");
        received += (size_t)count;
    }
    if ((msg.msg_flags & (MSG_CTRUNC | MSG_TRUNC)) || memcmp(response, "FMN1", 4))
        return fail(control, data, error, error_size, "Invalid network helper response");
    uint32_t status;
    memcpy(&status, response + 4, sizeof(status));
    status = ntohl(status);
    if (status != 0) {
        char message[256];
        if (status == EBUSY) snprintf(message, sizeof(message), "Mac network slot is already connected");
        else if (status == EACCES) snprintf(message, sizeof(message), "Mac network helper rejected the caller UID");
        else snprintf(message, sizeof(message), "Mac network helper rejected attachment (status %u)", status);
        return fail(control, data, error, error_size, message);
    }
    int type = 0;
    socklen_t length = sizeof(type);
    struct sockaddr_un peer = {0};
    socklen_t peer_length = sizeof(peer);
    if (fd_count != 1 || data < 0 || getsockopt(data, SOL_SOCKET, SO_TYPE, &type, &length) < 0 ||
        type != SOCK_DGRAM || getpeername(data, (struct sockaddr *)&peer, &peer_length) < 0 || peer.sun_family != AF_UNIX)
        return fail(control, data, error, error_size, "Network helper did not supply a connected Unix datagram socket");
    fcntl(data, F_SETFD, FD_CLOEXEC);
    *control_fd = control;
    return data;
}
