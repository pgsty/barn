#ifndef FARROW_MAC_NETWORK_CLIENT_H
#define FARROW_MAC_NETWORK_CLIENT_H
#include <stddef.h>

// Connect to the privileged helper. Returns a connected SOCK_DGRAM descriptor
// for VZFileHandleNetworkDeviceAttachment, or -1 with a bounded error string.
// Keep *control_fd open for the VM lifetime; closing it releases the interface.
// Both returned descriptors are close-on-exec. The caller owns them.
int farrow_mac_network_connect(const char *path, int slot, int *control_fd,
                               char *error, size_t error_size);
#endif
