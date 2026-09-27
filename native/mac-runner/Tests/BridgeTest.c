#include "../Bridge.h"
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>

int main(void) {
    char root[] = "/tmp/farrow-bridge-test.XXXXXX";
    assert(mkdtemp(root));
    char path[104];
    snprintf(path, sizeof(path), "%s/control.sock", root);
    char error[512];
    int server = fm_unix_listen(path, error, sizeof(error));
    assert(server >= 0);
    assert(fm_unix_listen(path, error, sizeof(error)) < 0);
    int client = fm_unix_connect(path, error, sizeof(error));
    assert(client >= 0);
    close(client); close(server); unlink(path);
    int lock = fm_lock(path);
    assert(lock >= 0);
    assert(fm_lock(path) < 0);
    close(lock); unlink(path);
    int regular = open(path, O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(regular >= 0);
    assert(fm_unix_listen(path, error, sizeof(error)) < 0);
    assert(access(path, F_OK) == 0);
    close(regular); unlink(path); rmdir(root);
    puts("{\"ok\":true,\"tests\":[\"rpc_socket_connection\",\"refuse_duplicate_listener_and_lock\",\"preserve_non_socket_path\"]}");
    return 0;
}
