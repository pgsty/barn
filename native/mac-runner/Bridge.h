#include <stddef.h>
#include <sys/types.h>
int fm_unix_listen(const char *path, char *error, size_t error_len);
int fm_unix_accept(int server);
int fm_unix_connect(const char *path, char *error, size_t error_len);
#include "../mac-network/client.h"
int fm_lock(const char *path);
