#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>
#include <vmnet/vmnet.h>
int fm_unix_listen(const char *path, char *error, size_t error_len);
int fm_unix_accept(int server);
int fm_unix_connect(const char *path, char *error, size_t error_len);
int fm_lock(const char *path);
// Create a private shared-mode vmnet network owned by this process. The host
// takes the gateway (.1); the guest MAC receives one DHCP reservation. The
// network lives as long as the returned reference. NULL sets *status.
vmnet_network_ref fm_vmnet_network_create(const char *gateway, const char *address,
                                          const char *mac, uint32_t *status);
