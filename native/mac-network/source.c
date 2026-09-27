#include "source.h"
#include <string.h>

static uint16_t read16(const uint8_t *p) {
    return (uint16_t)(((uint16_t)p[0] << 8) | p[1]);
}

enum fmn_source_protocol fmn_observe_source(const uint8_t *frame, size_t length,
                                           const uint8_t mac[6], const uint8_t ip[4]) {
    if (!frame || !mac || !ip || length < 14 || memcmp(frame + 6, mac, 6)) return FMN_SOURCE_NONE;
    const uint8_t *body = frame + 14;
    size_t available = length - 14;
    switch (read16(frame + 12)) {
        case 0x0800: {
            if (available < 20 || (body[0] >> 4) != 4) return FMN_SOURCE_NONE;
            size_t header = (body[0] & 15) * 4;
            size_t total = read16(body + 2);
            if (header < 20 || header > available || total < header || total > available ||
                memcmp(body + 12, ip, 4)) return FMN_SOURCE_NONE;
            uint32_t sum = 0;
            for (size_t n = 0; n < header; n += 2) sum += read16(body + n);
            while (sum >> 16) sum = (sum & 65535) + (sum >> 16);
            if (sum != 65535) return FMN_SOURCE_NONE;
            return FMN_SOURCE_IPV4;
        }
        case 0x0806:
            // ARP probes with sender 0.0.0.0 deliberately establish no evidence.
            if (available < 28 || read16(body) != 1 || read16(body + 2) != 0x0800 ||
                body[4] != 6 || body[5] != 4 || (read16(body + 6) != 1 && read16(body + 6) != 2) ||
                memcmp(body + 8, mac, 6) || memcmp(body + 14, ip, 4)) return FMN_SOURCE_NONE;
            return FMN_SOURCE_ARP;
        default:
            // No VLAN, IPv6 or unknown-EtherType inference.
            return FMN_SOURCE_NONE;
    }
}
