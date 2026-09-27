#ifndef FARROW_MAC_NETWORK_SOURCE_H
#define FARROW_MAC_NETWORK_SOURCE_H

#include <stddef.h>
#include <stdint.h>

enum fmn_source_protocol {
    FMN_SOURCE_NONE = 0,
    FMN_SOURCE_IPV4 = 1,
    FMN_SOURCE_ARP = 2
};

// Identifies an actual guest frame whose Ethernet source, and ARP sender when
// present, match the reserved MAC and IPv4 address. IP bytes use network order.
// This association is evidence of slot traffic, not SSH server authentication.
enum fmn_source_protocol fmn_observe_source(const uint8_t *frame, size_t length,
                                           const uint8_t mac[6], const uint8_t ip[4]);

#endif
