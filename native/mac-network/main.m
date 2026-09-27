#import <Foundation/Foundation.h>
#import <vmnet/vmnet.h>
#include <arpa/inet.h>
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <net/ethernet.h>
#include <pwd.h>
#include <signal.h>
#include <stdatomic.h>
#include <sys/socket.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>
#include "source.h"
#ifndef FARROW_NETWORK_BUILD_ID
#error Build with build.sh to embed the network helper source identity
#endif

// This process is the complete privileged surface: one immutable private /24,
// two reserved MAC/IP pairs, and two packet relays. It never launches commands,
// opens VM disks, or accepts a client-selected path, subnet, MAC, or IP.
static dispatch_queue_t networkQueue;
static vmnet_network_ref network;
static interface_ref anchorInterface;
static size_t anchorMaxPacket = 1514;
static id activeSlots[2];
static int listener = -1;
static NSString *boundSocket;
static atomic_int pendingClients;
static dispatch_source_t listenerSource;
static NSMutableArray *signalSources;
static dispatch_source_t maintenanceSource;

static void emit(NSDictionary *object) {
    NSData *data = [NSJSONSerialization dataWithJSONObject:object options:NSJSONWritingSortedKeys error:NULL];
    if (data) { fwrite(data.bytes, 1, data.length, stdout); fputc('\n', stdout); fflush(stdout); }
}
static BOOL errorMessage(NSString **error, NSString *message) { if (error) *error = message; return NO; }
static BOOL safeRootPath(NSString *path, NSString **error) {
    char resolved[PATH_MAX];
    if (!realpath(path.fileSystemRepresentation, resolved)) return errorMessage(error, @"Cannot resolve privileged path");
    NSString *part = [NSString stringWithUTF8String:resolved];
    for (;;) {
        struct stat st;
        if (lstat(part.fileSystemRepresentation, &st)) return errorMessage(error, @"Cannot inspect privileged path");
        // macOS ships /private/var/run as root:daemon 0775. Trust exactly that
        // system ancestor, not arbitrary group-writable directories or the
        // application's own socket directory. Ordinary users are not daemon.
        BOOL systemRun = [part isEqualToString:@"/private/var/run"] && S_ISDIR(st.st_mode) &&
            st.st_uid == 0 && st.st_gid == 1 && (st.st_mode & 07777) == 0775;
        if (st.st_uid != 0 || ((st.st_mode & 0022) && !systemRun))
            return errorMessage(error, [NSString stringWithFormat:@"Privileged path must be root owned and not group/world writable: %@", part]);
        if ([part isEqualToString:@"/"]) break;
        part = part.stringByDeletingLastPathComponent;
    }
    return YES;
}

@interface NetworkConfig : NSObject
@property uid_t uid;
@property uint32_t subnet;
@property(strong) NSString *installationID;
@property(strong) NSArray<NSData *> *macs;
@property(strong) NSDictionary *json;
@end
@implementation NetworkConfig
@end

static NetworkConfig *loadConfig(NSString *path, BOOL trusted, NSString **error) {
    if (trusted && !safeRootPath(path, error)) return nil;
    int fd = open(path.fileSystemRepresentation, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) { errorMessage(error, @"Cannot open network configuration (symlinks are not accepted)"); return nil; }
    struct stat st;
    if (fstat(fd, &st) || !S_ISREG(st.st_mode) || st.st_size < 2 || st.st_size > 16384 ||
        (trusted && (st.st_uid != 0 || (st.st_mode & 0022)))) {
        close(fd); errorMessage(error, @"Network configuration must be a small regular file with secure ownership"); return nil;
    }
    NSMutableData *data = [NSMutableData dataWithLength:(NSUInteger)st.st_size];
    size_t used = 0;
    while (used < data.length) {
        ssize_t n = read(fd, (char *)data.mutableBytes + used, data.length - used);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) break;
        used += (size_t)n;
    }
    close(fd);
    id json = used == data.length ? [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL] : nil;
    if (![json isKindOfClass:NSDictionary.class]) { errorMessage(error, @"Invalid network JSON"); return nil; }
    NSDictionary *d = json;
    NSSet *expected = [NSSet setWithArray:@[@"schema_version", @"uid", @"installation_id", @"subnet", @"slots"]];
    id schema = d[@"schema_version"];
    if (![[NSSet setWithArray:d.allKeys] isEqual:expected] || ![schema isKindOfClass:NSNumber.class] ||
        CFGetTypeID((__bridge CFTypeRef)schema) == CFBooleanGetTypeID() || ![schema isEqual:@1]) {
        errorMessage(error, @"Expected network schema_version 1 and only the documented fields"); return nil;
    }
    NSNumber *uid = d[@"uid"];
    if (![uid isKindOfClass:NSNumber.class] || CFGetTypeID((__bridge CFTypeRef)uid) == CFBooleanGetTypeID() ||
        uid.doubleValue != uid.unsignedIntValue || uid.unsignedIntValue < 501 || !getpwuid(uid.unsignedIntValue)) {
        errorMessage(error, @"Network uid must identify an existing ordinary local user (uid >= 501)"); return nil;
    }
    NSString *identifier = d[@"installation_id"];
    if (![identifier isKindOfClass:NSString.class] || ![[NSUUID alloc] initWithUUIDString:identifier]) {
        errorMessage(error, @"installation_id must be a UUID"); return nil;
    }
    NSString *subnet = d[@"subnet"];
    if (![subnet isKindOfClass:NSString.class] || ![subnet hasSuffix:@"/24"]) {
        errorMessage(error, @"Network subnet must be an RFC1918 IPv4 /24"); return nil;
    }
    struct in_addr address;
    if (inet_pton(AF_INET, [subnet substringToIndex:subnet.length - 3].UTF8String, &address) != 1) {
        errorMessage(error, @"Invalid IPv4 subnet"); return nil;
    }
    uint32_t host = ntohl(address.s_addr);
    BOOL private = (host & 0xff000000) == 0x0a000000 || (host & 0xfff00000) == 0xac100000 || (host & 0xffff0000) == 0xc0a80000;
    if (!private || (host & 0xff)) { errorMessage(error, @"Expected the network address of a private /24 (last octet 0)"); return nil; }
    NSArray *slots = d[@"slots"];
    if (![slots isKindOfClass:NSArray.class] || slots.count != 2) { errorMessage(error, @"Exactly mac1 and mac2 reservations are required"); return nil; }
    NSMutableArray *macs = [NSMutableArray array];
    for (unsigned i = 0; i < 2; ++i) {
        id slot = slots[i];
        if (![slot isKindOfClass:NSDictionary.class] || ![[NSSet setWithArray:[slot allKeys]] isEqual:[NSSet setWithArray:@[@"name", @"mac", @"ip"]]] ||
            ![slot[@"name"] isEqual:[NSString stringWithFormat:@"mac%u", i + 1]] ||
            ![slot[@"mac"] isKindOfClass:NSString.class] || ![slot[@"ip"] isKindOfClass:NSString.class]) {
            errorMessage(error, @"Invalid slot reservation; expected ordered mac1, mac2 with name, mac, ip"); return nil;
        }
        unsigned char bytes[6];
        NSString *mac = slot[@"mac"];
        if (mac.length != 17) { errorMessage(error, @"Invalid Ethernet MAC address"); return nil; }
        for (unsigned j = 0; j < 6; ++j) {
            char pair[3] = { [mac UTF8String][j * 3], [mac UTF8String][j * 3 + 1], 0 };
            char *end;
            unsigned long byte = strtoul(pair, &end, 16);
            if (end != pair + 2 || !isxdigit(pair[0]) || !isxdigit(pair[1]) || (j < 5 && [mac UTF8String][j * 3 + 2] != ':')) {
                errorMessage(error, @"Invalid Ethernet MAC address"); return nil;
            }
            bytes[j] = (unsigned char)byte;
        }
        if ((bytes[0] & 3) != 2) { errorMessage(error, @"Slot MAC must be locally administered and unicast"); return nil; }
        NSData *macData = [NSData dataWithBytes:bytes length:6];
        if ([macs containsObject:macData]) { errorMessage(error, @"Slot MAC addresses must be distinct"); return nil; }
        [macs addObject:macData];
        struct in_addr ip;
        if (inet_pton(AF_INET, [slot[@"ip"] UTF8String], &ip) != 1 || ntohl(ip.s_addr) != host + 10 + i) {
            errorMessage(error, @"mac1 and mac2 must reserve subnet addresses .10 and .11"); return nil;
        }
    }
    NetworkConfig *config = [NetworkConfig new];
    config.uid = uid.unsignedIntValue; config.installationID = identifier.lowercaseString;
    config.subnet = host; config.macs = macs; config.json = d;
    return config;
}

static NSString *socketPath(NetworkConfig *config) {
    return [NSString stringWithFormat:@"/var/run/farrow-mac/%u-%@.sock", config.uid, config.installationID];
}
static NSString *serviceLabel(NetworkConfig *config) {
    return [NSString stringWithFormat:@"io.pgsty.farrow.mac-network.%u.%@", config.uid, config.installationID];
}
static int acquireNetworkLock(NSString *path) {
    NSString *lockPath = [path stringByAppendingString:@".lock"];
    int fd = open(lockPath.fileSystemRepresentation, O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0600);
    struct stat st;
    if (fd < 0 || fstat(fd, &st) || st.st_uid != 0 || !S_ISREG(st.st_mode) || (st.st_mode & 0022) || flock(fd, LOCK_EX | LOCK_NB)) {
        if (fd >= 0) close(fd);
        return -1;
    }
    return fd;
}
static int holdRepairLock(NetworkConfig *config, NSString *path, BOOL administrative) {
    NSString *error = nil;
    if (geteuid() != 0 || ![path isEqualToString:socketPath(config)] || !safeRootPath(path.stringByDeletingLastPathComponent, &error)) {
        emit(@{@"ok": @NO, @"error": error ?: @"Root and the exact installation socket path are required"}); return 77;
    }
    int fd = acquireNetworkLock(administrative ? [path stringByAppendingString:@".admin"] : path);
    if (fd < 0) { emit(@{@"ok": @NO, @"error": @"Active daemon lock or unsafe lock path; repair refused"}); return 75; }
    emit(@{@"ok": @YES, administrative ? @"admin_lock_held" : @"repair_lock_held": @YES, @"pid": @(getpid())});
    // The installer holds the writing end of a private FIFO. EOF also releases
    // this lock when the installer crashes, without leaving a lock service.
    char byte;
    while (read(STDIN_FILENO, &byte, 1) > 0 || errno == EINTR) { errno = 0; }
    close(fd);
    return 0;
}
static vmnet_network_ref createNetwork(NetworkConfig *c, vmnet_return_t *status) {
    vmnet_network_configuration_ref cfg = vmnet_network_configuration_create(VMNET_SHARED_MODE, status);
    if (!cfg) return NULL;
    // NetworkSharing uses this address as the host gateway. Passing the masked
    // .0 network address passes configuration validation but fails DHCP at
    // interface start ("can't have the gateway at the subnet address").
    // Keep .0/24 in persisted state; request the host's .1 here.
    struct in_addr subnet = { .s_addr = htonl(c.subnet + 1) }, mask = { .s_addr = htonl(0xffffff00) };
    *status = vmnet_network_configuration_set_ipv4_subnet(cfg, &subnet, &mask);
    for (unsigned i = 0; i < 2 && *status == VMNET_SUCCESS; ++i) {
        ether_addr_t mac; memcpy(&mac, c.macs[i].bytes, 6);
        struct in_addr ip = { .s_addr = htonl(c.subnet + 10 + i) };
        *status = vmnet_network_configuration_add_dhcp_reservation(cfg, &mac, &ip);
    }
    vmnet_network_ref result = *status == VMNET_SUCCESS ? vmnet_network_create(cfg, status) : NULL;
    CFRelease(cfg);
    return result;
}

static BOOL sendReply(int fd, uint32_t status, int dataFD) {
    unsigned char bytes[8] = {'F', 'M', 'N', '1', 0, 0, 0, 0};
    uint32_t encoded = htonl(status); memcpy(bytes + 4, &encoded, 4);
    struct iovec iov = { .iov_base = bytes, .iov_len = sizeof(bytes) };
    union { struct cmsghdr alignment; unsigned char bytes[CMSG_SPACE(sizeof(int))]; } control;
    memset(&control, 0, sizeof(control));
    struct msghdr msg = { .msg_iov = &iov, .msg_iovlen = 1 };
    if (dataFD >= 0) {
        msg.msg_control = control.bytes; msg.msg_controllen = sizeof(control.bytes);
        struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg);
        cmsg->cmsg_level = SOL_SOCKET; cmsg->cmsg_type = SCM_RIGHTS; cmsg->cmsg_len = CMSG_LEN(sizeof(int));
        memcpy(CMSG_DATA(cmsg), &dataFD, sizeof(int));
    }
    ssize_t n;
    do { n = sendmsg(fd, &msg, 0); } while (n < 0 && errno == EINTR);
    if (n <= 0) return NO;
    size_t sent = (size_t)n;
    while (sent < sizeof(bytes)) {
        n = send(fd, bytes + sent, sizeof(bytes) - sent, 0);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return NO;
        sent += (size_t)n;
    }
    return YES;
}

static NSArray *slotSources(NetworkConfig *config);

static void replyStatus(int fd, NetworkConfig *config) {
    if (!sendReply(fd, 0, -1)) { close(fd); return; }
    NSArray *sources = slotSources(config);
    NSDictionary *reply = @{@"ok": @YES, @"protocol": @1, @"uid": @(config.uid),
        @"build_id": @FARROW_NETWORK_BUILD_ID,
        @"installation_id": config.installationID, @"subnet": config.json[@"subnet"],
        @"socket": socketPath(config), @"pid": @(getpid()),
        @"connected": @[sources[0][@"connected"], sources[1][@"connected"]],
        @"slot_sources": sources,
        @"network_active": anchorInterface ? @YES : @NO,
        @"packet_counters": @[
            activeSlots[0] ? [activeSlots[0] dictionaryWithValuesForKeys:@[@"framesFromVM", @"framesToVM", @"dropped", @"lastReadStatus", @"lastWriteStatus"]] : NSNull.null,
            activeSlots[1] ? [activeSlots[1] dictionaryWithValuesForKeys:@[@"framesFromVM", @"framesToVM", @"dropped", @"lastReadStatus", @"lastWriteStatus"]] : NSNull.null],
        @"maintenance": maintenanceSource ? @YES : @NO, @"supports_maintenance": @YES};
    NSData *json = [NSJSONSerialization dataWithJSONObject:reply options:NSJSONWritingSortedKeys error:NULL];
    size_t sent = 0;
    while (sent < json.length) {
        ssize_t count = send(fd, (const char *)json.bytes + sent, json.length - sent, 0);
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) break;
        sent += (size_t)count;
    }
    close(fd);
}

static void beginMaintenance(int fd) {
    if (activeSlots[0] || activeSlots[1] || maintenanceSource) { sendReply(fd, EBUSY, -1); close(fd); return; }
    if (!sendReply(fd, 0, -1)) { close(fd); return; }
    fcntl(fd, F_SETFL, O_NONBLOCK);
    maintenanceSource = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, fd, 0, networkQueue);
    dispatch_source_set_cancel_handler(maintenanceSource, ^{ close(fd); });
    dispatch_source_set_event_handler(maintenanceSource, ^{
        char byte;
        ssize_t n = recv(fd, &byte, 1, MSG_DONTWAIT);
        if (n >= 0 || (errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR)) {
            dispatch_source_cancel(maintenanceSource);
            maintenanceSource = nil;
        }
    });
    dispatch_resume(maintenanceSource);
}

static int holdMaintenanceLease(NSString *path) {
    if (geteuid() != 0) { emit(@{@"ok": @NO, @"error": @"Maintenance requires root"}); return 77; }
    struct sockaddr_un address = { .sun_family = AF_UNIX, .sun_len = sizeof(address) };
    if (strlen(path.fileSystemRepresentation) >= sizeof(address.sun_path)) return 64;
    strcpy(address.sun_path, path.fileSystemRepresentation);
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    struct timeval timeout = { .tv_sec = 5 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    int yes = 1; setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &yes, sizeof(yes));
    uid_t uid; gid_t gid;
    if (fd < 0 || connect(fd, (struct sockaddr *)&address, sizeof(address)) || getpeereid(fd, &uid, &gid) || uid != 0) {
        if (fd >= 0) close(fd);
        emit(@{@"ok": @NO, @"error": @"Cannot authenticate network daemon for maintenance"}); return 69;
    }
    unsigned char request[8] = {'F', 'M', 'N', '1', 3, 0, 0, 0}, response[8] = {0};
    if (send(fd, request, sizeof(request), 0) != sizeof(request) || recv(fd, response, sizeof(response), MSG_WAITALL) != sizeof(response) ||
        memcmp(response, "FMN1", 4)) {
        close(fd); emit(@{@"ok": @NO, @"error": @"Network maintenance handshake failed"}); return 71;
    }
    uint32_t status; memcpy(&status, response + 4, 4); status = ntohl(status);
    if (status) { close(fd); emit(@{@"ok": @NO, @"error": @"Network is busy or maintenance unsupported", @"status": @(status)}); return 75; }
    emit(@{@"ok": @YES, @"maintenance_lease_held": @YES, @"pid": @(getpid())});
    char byte;
    for (;;) {
        ssize_t n = read(STDIN_FILENO, &byte, 1);
        if (n > 0 || (n < 0 && errno == EINTR)) continue;
        break;
    }
    close(fd); return 0;
}

static int queryStatus(NSString *path) {
    struct sockaddr_un address = { .sun_family = AF_UNIX, .sun_len = sizeof(address) };
    if (strlen(path.fileSystemRepresentation) >= sizeof(address.sun_path)) return 64;
    strcpy(address.sun_path, path.fileSystemRepresentation);
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    struct timeval timeout = { .tv_sec = 5 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    int yes = 1; setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &yes, sizeof(yes));
    uid_t uid; gid_t gid;
    if (fd < 0 || connect(fd, (struct sockaddr *)&address, sizeof(address)) || getpeereid(fd, &uid, &gid) || uid != 0) {
        if (fd >= 0) close(fd);
        emit(@{@"ok": @NO, @"error": @"Mac network helper is unavailable or peer is not root", @"socket": path}); return 69;
    }
    unsigned char request[8] = {'F', 'M', 'N', '1', 0, 0, 0, 0}, response[8];
    if (send(fd, request, sizeof(request), 0) != sizeof(request) || recv(fd, response, sizeof(response), MSG_WAITALL) != sizeof(response) ||
        memcmp(response, request, sizeof(request))) {
        close(fd); emit(@{@"ok": @NO, @"error": @"Network helper status handshake failed"}); return 71;
    }
    NSMutableData *data = [NSMutableData data];
    char buffer[1024];
    for (;;) {
        ssize_t n = recv(fd, buffer, sizeof(buffer), 0);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) break;
        [data appendBytes:buffer length:(NSUInteger)n];
        if (data.length > 16384) break;
    }
    close(fd);
    id json = data.length <= 16384 ? [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL] : nil;
    if (![json isKindOfClass:NSDictionary.class] || ![json[@"ok"] isEqual:@YES]) {
        emit(@{@"ok": @NO, @"error": @"Invalid network helper status response"}); return 71;
    }
    emit(json); return 0;
}

@interface Session : NSObject
@property int controlFD;
@property int dataFD;
@property int slot;
@property interface_ref interface;
@property size_t maxPacketSize;
@property BOOL closed;
@property uint64_t dropped;
@property uint64_t framesFromVM;
@property uint64_t framesToVM;
@property vmnet_return_t lastReadStatus;
@property vmnet_return_t lastWriteStatus;
@property uint32_t reservedIP;
@property enum fmn_source_protocol observedSource;
@property(strong) NSData *mac;
@property(strong) dispatch_source_t dataSource;
@property(strong) dispatch_source_t controlSource;
- (void)close;
- (void)readVM;
- (void)readNetwork;
@end
@implementation Session
- (instancetype)init { if ((self = [super init])) { _dataFD = -1; _controlFD = -1; } return self; }
- (void)close {
    if (self.closed) return;
    self.closed = YES;
    self.observedSource = FMN_SOURCE_NONE;
    if (self.dataSource) { dispatch_source_cancel(self.dataSource); self.dataSource = nil; }
    else if (self.dataFD >= 0) close(self.dataFD);
    if (self.controlSource) { dispatch_source_cancel(self.controlSource); self.controlSource = nil; }
    else if (self.controlFD >= 0) close(self.controlFD);
    self.dataFD = -1; self.controlFD = -1;
    emit(@{@"event": @"detached", @"slot": @(self.slot + 1), @"dropped_packets": @(self.dropped),
           @"frames_from_vm": @(self.framesFromVM), @"frames_to_vm": @(self.framesToVM),
           @"last_read_status": @(self.lastReadStatus), @"last_write_status": @(self.lastWriteStatus)});
    if (self.interface) {
        interface_ref iface = self.interface;
        self.interface = NULL;
        vmnet_interface_set_event_callback(iface, VMNET_INTERFACE_PACKETS_AVAILABLE, NULL, NULL);
        vmnet_stop_interface(iface, networkQueue, ^(vmnet_return_t status) {
            if (status != VMNET_SUCCESS) emit(@{@"event": @"stop_error", @"status": @(status)});
            activeSlots[self.slot] = nil;
        });
    } else { activeSlots[self.slot] = nil; }
}
- (void)readVM {
    if (self.closed || !self.interface) return;
    unsigned char buffer[65536];
    for (int n = 0; n < 64; ++n) {
        struct iovec iov = { .iov_base = buffer, .iov_len = sizeof(buffer) };
        struct msghdr msg = { .msg_iov = &iov, .msg_iovlen = 1 };
        ssize_t size = recvmsg(self.dataFD, &msg, MSG_DONTWAIT);
        if (size < 0 && errno == EINTR) { --n; continue; }
        if (size < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return;
        if (size < 0) { [self close]; return; }
        self.framesFromVM++;
        if (size < 14 || (size_t)size > self.maxPacketSize || (msg.msg_flags & MSG_TRUNC) || memcmp(buffer + 6, self.mac.bytes, 6)) {
            self.dropped++; continue;
        }
        enum fmn_source_protocol source = fmn_observe_source(buffer, (size_t)size, self.mac.bytes,
                                                           (const uint8_t *)&_reservedIP);
        if (source != FMN_SOURCE_NONE) self.observedSource = source;
        iov.iov_len = (size_t)size;
        struct vmpktdesc packet = { .vm_pkt_size = (size_t)size, .vm_pkt_iov = &iov, .vm_pkt_iovcnt = 1 };
        int count = 1;
        vmnet_return_t status = vmnet_write(self.interface, &packet, &count);
        self.lastWriteStatus = status;
        if (status != VMNET_SUCCESS || count != 1) self.dropped++;
    }
    // Bounded work on the shared queue; the dispatch source remains readable.
}
- (void)readNetwork {
    if (self.closed || !self.interface) return;
    unsigned char buffer[65536];
    for (int n = 0; n < 64; ++n) {
        struct iovec iov = { .iov_base = buffer, .iov_len = self.maxPacketSize };
        struct vmpktdesc packet = { .vm_pkt_size = self.maxPacketSize, .vm_pkt_iov = &iov, .vm_pkt_iovcnt = 1 };
        int count = 1;
        vmnet_return_t status = vmnet_read(self.interface, &packet, &count);
        if (status != VMNET_SUCCESS && status != self.lastReadStatus)
            emit(@{@"event": @"vmnet_read_error", @"slot": @(self.slot + 1), @"vmnet_status": @(status), @"packet_count": @(count)});
        self.lastReadStatus = status;
        if (status != VMNET_SUCCESS || count == 0) return;
        if (packet.vm_pkt_size < 14 || packet.vm_pkt_size > self.maxPacketSize) { self.dropped++; continue; }
        ssize_t sent = send(self.dataFD, buffer, packet.vm_pkt_size, MSG_DONTWAIT);
        if (sent == (ssize_t)packet.vm_pkt_size) self.framesToVM++;
        if (sent != (ssize_t)packet.vm_pkt_size) {
            self.dropped++;
            if (sent < 0 && errno != EAGAIN && errno != EWOULDBLOCK && errno != ENOBUFS && errno != EINTR) { [self close]; return; }
        }
    }
    dispatch_async(networkQueue, ^{ [self readNetwork]; });
}
@end

static NSArray *slotSources(NetworkConfig *config) {
    NSMutableArray *sources = [NSMutableArray arrayWithCapacity:2];
    for (unsigned i = 0; i < 2; ++i) {
        Session *session = activeSlots[i];
        BOOL connected = session && !session.closed && session.dataFD >= 0;
        NSDictionary *reservation = config.json[@"slots"][i];
        NSString *mac = [reservation[@"mac"] lowercaseString];
        id observed = NSNull.null;
        if (connected && session.observedSource != FMN_SOURCE_NONE) {
            observed = @{@"ip": reservation[@"ip"], @"mac": mac,
                @"protocol": session.observedSource == FMN_SOURCE_IPV4 ? @"ipv4" : @"arp"};
        }
        [sources addObject:@{@"slot": reservation[@"name"], @"connected": connected ? @YES : @NO,
            @"reserved_ip": reservation[@"ip"], @"reserved_mac": mac, @"observed_source": observed}];
    }
    return sources;
}

static void drainAnchor(void) {
    if (!anchorInterface) return;
    unsigned char bytes[65536];
    for (unsigned i = 0; i < 64; ++i) {
        struct iovec iov = { .iov_base = bytes, .iov_len = anchorMaxPacket };
        struct vmpktdesc packet = { .vm_pkt_size = anchorMaxPacket, .vm_pkt_iov = &iov, .vm_pkt_iovcnt = 1 };
        int count = 1;
        if (vmnet_read(anchorInterface, &packet, &count) != VMNET_SUCCESS || count == 0) return;
    }
    dispatch_async(networkQueue, ^{ drainAnchor(); });
}

static void attachClient(int fd, unsigned slot, NetworkConfig *config) {
    if (activeSlots[slot] || maintenanceSource) { sendReply(fd, EBUSY, -1); close(fd); return; }
    Session *session = [Session new];
    session.controlFD = fd; session.slot = (int)slot; session.mac = config.macs[slot];
    session.reservedIP = htonl(config.subnet + 10 + slot);
    activeSlots[slot] = session;
    xpc_object_t desc = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_bool(desc, vmnet_allocate_mac_address_key, false);
    xpc_dictionary_set_bool(desc, vmnet_enable_tso_key, false);
    xpc_dictionary_set_bool(desc, vmnet_enable_checksum_offload_key, false);
    session.interface = vmnet_interface_start_with_network(network, desc, networkQueue, ^(vmnet_return_t status, xpc_object_t params) {
        if (status != VMNET_SUCCESS || !params) { sendReply(fd, status, -1); [session close]; return; }
        session.maxPacketSize = (size_t)xpc_dictionary_get_uint64(params, vmnet_max_packet_size_key);
        if (session.maxPacketSize < 1514 || session.maxPacketSize > 65536) { sendReply(fd, EMSGSIZE, -1); [session close]; return; }
        int pair[2];
        if (socketpair(AF_UNIX, SOCK_DGRAM, 0, pair)) { sendReply(fd, errno, -1); [session close]; return; }
        session.dataFD = pair[0];
        int sendSize = 65536, receiveSize = 262144, yes = 1;
        BOOL configured = YES;
        for (int i = 0; i < 2; ++i) {
            if (fcntl(pair[i], F_SETFD, FD_CLOEXEC) || setsockopt(pair[i], SOL_SOCKET, SO_SNDBUF, &sendSize, sizeof(sendSize)) ||
                setsockopt(pair[i], SOL_SOCKET, SO_RCVBUF, &receiveSize, sizeof(receiveSize)) ||
                setsockopt(pair[i], SOL_SOCKET, SO_NOSIGPIPE, &yes, sizeof(yes))) configured = NO;
        }
        if (!configured) { sendReply(fd, errno ?: ENOBUFS, -1); close(pair[1]); [session close]; return; }
        vmnet_return_t eventStatus = vmnet_interface_set_event_callback(session.interface, VMNET_INTERFACE_PACKETS_AVAILABLE, networkQueue,
            ^(interface_event_t event, xpc_object_t value) { (void)event; (void)value; [session readNetwork]; });
        if (eventStatus != VMNET_SUCCESS) { sendReply(fd, eventStatus, -1); close(pair[1]); [session close]; return; }
        if (!sendReply(fd, 0, pair[1])) { close(pair[1]); [session close]; return; }
        close(pair[1]);
        fcntl(session.dataFD, F_SETFL, O_NONBLOCK);
        fcntl(fd, F_SETFL, O_NONBLOCK);
        session.dataSource = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, session.dataFD, 0, networkQueue);
        int relayFD = session.dataFD;
        dispatch_source_set_cancel_handler(session.dataSource, ^{ close(relayFD); });
        dispatch_source_set_event_handler(session.dataSource, ^{ [session readVM]; });
        dispatch_resume(session.dataSource);
        session.controlSource = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, fd, 0, networkQueue);
        dispatch_source_set_cancel_handler(session.controlSource, ^{ close(fd); });
        dispatch_source_set_event_handler(session.controlSource, ^{
            char unexpected;
            ssize_t n = recv(fd, &unexpected, 1, MSG_DONTWAIT);
            if (n >= 0 || (errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR)) [session close];
        });
        dispatch_resume(session.controlSource);
        emit(@{@"event": @"attached", @"slot": @(slot + 1), @"max_packet_size": @(session.maxPacketSize)});
    });
    if (!session.interface) { sendReply(fd, VMNET_FAILURE, -1); [session close]; }
}

static int serve(NetworkConfig *config, NSString *path) {
    if (geteuid() != 0) { emit(@{@"ok": @NO, @"error": @"Root authorization is required; install the Mac network helper during farrow mac setup"}); return 77; }
    if (![path isEqualToString:socketPath(config)]) { emit(@{@"ok": @NO, @"error": @"Socket path does not match the installation namespace", @"expected": socketPath(config)}); return 64; }
    NSString *error = nil;
    if (!safeRootPath(path.stringByDeletingLastPathComponent, &error)) { emit(@{@"ok": @NO, @"error": error}); return 77; }
    // A root-owned per-network lock prevents duplicate daemons and stale socket
    // removal racing another process. It contains no user-controlled filename.
    int lockFD = acquireNetworkLock(path);
    if (lockFD < 0) {
        emit(@{@"ok": @NO, @"error": @"Mac network daemon is already running or lock is unsafe"}); return 73;
    }
    vmnet_return_t status;
    network = createNetwork(config, &status);
    if (!network) { emit(@{@"ok": @NO, @"phase": @"vmnet_network_create", @"vmnet_status": @(status)}); return 71; }
    // Keep the configured DHCP service active while slots disconnect. A real
    // two-attach test on Golden Gate observed its reserved .10 offered first,
    // then dynamic .2 after the last interface stopped. This network-only
    // anchor consumes no macOS VM runtime slot and also proves interface setup
    // before readiness is reported to the CLI.
    dispatch_semaphore_t ready = dispatch_semaphore_create(0);
    __block vmnet_return_t anchorStatus = VMNET_SETUP_INCOMPLETE;
    xpc_object_t anchorDesc = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_bool(anchorDesc, vmnet_allocate_mac_address_key, false);
    anchorInterface = vmnet_interface_start_with_network(network, anchorDesc, networkQueue, ^(vmnet_return_t value, xpc_object_t params) {
        anchorStatus = value;
        if (params) anchorMaxPacket = (size_t)xpc_dictionary_get_uint64(params, vmnet_max_packet_size_key);
        dispatch_semaphore_signal(ready);
    });
    if (!anchorInterface || dispatch_semaphore_wait(ready, dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC)) || anchorStatus != VMNET_SUCCESS) {
        emit(@{@"ok": @NO, @"phase": @"anchor_interface_start", @"vmnet_status": @(anchorStatus)}); return 71;
    }
    if (anchorMaxPacket < 1514 || anchorMaxPacket > 65536 ||
        vmnet_interface_set_event_callback(anchorInterface, VMNET_INTERFACE_PACKETS_AVAILABLE, networkQueue,
            ^(interface_event_t event, xpc_object_t params) { (void)event; (void)params; drainAnchor(); }) != VMNET_SUCCESS) {
        emit(@{@"ok": @NO, @"phase": @"anchor_packet_handler"}); return 71;
    }
    struct sockaddr_un address = { .sun_family = AF_UNIX, .sun_len = sizeof(address) };
    if (strlen(path.fileSystemRepresentation) >= sizeof(address.sun_path)) return 64;
    strcpy(address.sun_path, path.fileSystemRepresentation);
    struct stat old;
    if (!lstat(path.fileSystemRepresentation, &old)) {
        if (!S_ISSOCK(old.st_mode) || old.st_uid != config.uid || unlink(path.fileSystemRepresentation)) {
            emit(@{@"ok": @NO, @"error": @"Refusing to replace an unexpected socket path"}); return 73;
        }
    } else if (errno != ENOENT) return 73;
    listener = socket(AF_UNIX, SOCK_STREAM, 0);
    if (listener < 0 || bind(listener, (struct sockaddr *)&address, sizeof(address)) ||
        chown(path.fileSystemRepresentation, config.uid, -1) || chmod(path.fileSystemRepresentation, 0600) || listen(listener, 8)) {
        emit(@{@"ok": @NO, @"error": @"Cannot bind private network control socket", @"errno": @(errno)}); return 71;
    }
    boundSocket = path;
    fcntl(listener, F_SETFL, O_NONBLOCK); fcntl(listener, F_SETFD, FD_CLOEXEC);
    listenerSource = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, listener, 0, networkQueue);
    dispatch_source_set_event_handler(listenerSource, ^{
        for (;;) {
            int fd = accept(listener, NULL, NULL);
            if (fd < 0) break;
            if (atomic_fetch_add(&pendingClients, 1) >= 16) { atomic_fetch_sub(&pendingClients, 1); close(fd); continue; }
            fcntl(fd, F_SETFD, FD_CLOEXEC);
            int yes = 1; setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &yes, sizeof(yes));
            struct timeval deadline = { .tv_sec = 3 };
            setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &deadline, sizeof(deadline));
            setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &deadline, sizeof(deadline));
            dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
                uid_t uid; gid_t gid;
                unsigned char request[8] = {0};
                ssize_t length = -1;
                BOOL allowed = getpeereid(fd, &uid, &gid) == 0 && (uid == config.uid || uid == 0);
                if (allowed) length = recv(fd, request, sizeof(request), MSG_WAITALL);
                BOOL valid = allowed && length == sizeof(request) && !memcmp(request, "FMN1", 4) &&
                    request[4] <= 3 && request[5] == 0 && request[6] == 0 && request[7] == 0;
                BOOL statusOnly = request[4] == 0;
                BOOL maintenanceOnly = request[4] == 3;
                if (valid && ((!statusOnly && !maintenanceOnly && uid != config.uid) || (maintenanceOnly && uid != 0))) valid = NO;
                unsigned slot = request[4] - 1;
                dispatch_async(networkQueue, ^{
                    atomic_fetch_sub(&pendingClients, 1);
                    if (valid && statusOnly) replyStatus(fd, config);
                    else if (valid && maintenanceOnly) beginMaintenance(fd);
                    else if (valid) attachClient(fd, slot, config);
                    else { sendReply(fd, allowed ? EPROTO : EACCES, -1); close(fd); }
                });
            });
        }
    });
    dispatch_resume(listenerSource);
    signalSources = [NSMutableArray array];
    for (NSNumber *signalNumber in @[@(SIGTERM), @(SIGINT)]) {
        int number = signalNumber.intValue;
        signal(number, SIG_IGN);
        dispatch_source_t signalSource = dispatch_source_create(DISPATCH_SOURCE_TYPE_SIGNAL, (uintptr_t)number, 0, networkQueue);
        dispatch_source_set_event_handler(signalSource, ^{
            close(listener); unlink(boundSocket.fileSystemRepresentation);
            for (unsigned i = 0; i < 2; ++i) [(Session *)activeSlots[i] close];
            if (anchorInterface) {
                vmnet_stop_interface(anchorInterface, networkQueue, ^(vmnet_return_t value) { (void)value; });
                anchorInterface = NULL;
            }
            dispatch_after(dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC), networkQueue, ^{ exit(0); });
        });
        [signalSources addObject:signalSource];
        dispatch_resume(signalSource);
    }
    struct in_addr subnet, mask;
    vmnet_network_get_ipv4_subnet(network, &subnet, &mask);
    emit(@{@"ok": @YES, @"event": @"listening", @"socket": path, @"uid": @(config.uid),
           @"installation_id": config.installationID, @"subnet_address": [NSString stringWithUTF8String:inet_ntoa(subnet)], @"pid": @(getpid())});
    dispatch_main();
}

static int probe(NetworkConfig *config) {
    vmnet_return_t status;
    network = createNetwork(config, &status);
    if (!network) {
        emit(@{@"ok": @NO, @"phase": @"vmnet_network_create", @"uid": @(geteuid()), @"vmnet_status": @(status),
               @"required_action": @"Run the reviewed helper installation with administrator authorization; no restricted entitlement is claimed"});
        return 77;
    }
    vmnet_return_t serializationStatus;
    xpc_object_t serialization = vmnet_network_copy_serialization(network, &serializationStatus);
    vmnet_network_ref copy = serialization ? vmnet_network_create_with_serialization(serialization, &serializationStatus) : NULL;
    if (copy) CFRelease(copy);
    xpc_object_t desc = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_bool(desc, vmnet_allocate_mac_address_key, false);
    __block interface_ref interface;
    interface = vmnet_interface_start_with_network(network, desc, networkQueue, ^(vmnet_return_t startStatus, xpc_object_t params) {
        if (startStatus != VMNET_SUCCESS) {
            emit(@{@"ok": @NO, @"phase": @"vmnet_interface_start_with_network", @"vmnet_status": @(startStatus)}); exit(71);
        }
        emit(@{@"ok": @YES, @"phase": @"interface_started", @"uid": @(geteuid()), @"vmnet_status": @(startStatus),
               @"same_process_serialization_status": @(serializationStatus),
               @"mtu": @(xpc_dictionary_get_uint64(params, vmnet_mtu_key)),
               @"max_packet_size": @(xpc_dictionary_get_uint64(params, vmnet_max_packet_size_key))});
        vmnet_stop_interface(interface, networkQueue, ^(vmnet_return_t stopStatus) {
            emit(@{@"ok": stopStatus == VMNET_SUCCESS ? @YES : @NO, @"phase": @"interface_stopped", @"vmnet_status": @(stopStatus)});
            exit(stopStatus == VMNET_SUCCESS ? 0 : 71);
        });
    });
    if (!interface) { emit(@{@"ok": @NO, @"phase": @"interface_start_returned_null"}); return 71; }
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC), networkQueue, ^{
        emit(@{@"ok": @NO, @"phase": @"probe_timeout"}); exit(75);
    });
    dispatch_main();
}

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        umask(0077);
        if (argc == 2 && !strcmp(argv[1], "--version")) { emit(@{@"name": @"farrow-mac-network", @"protocol": @1, @"schema_version": @1, @"build_id": @FARROW_NETWORK_BUILD_ID}); return 0; }
        if (argc == 4 && !strcmp(argv[1], "status") && !strcmp(argv[2], "--socket")) return queryStatus(@(argv[3]));
        if (argc == 4 && !strcmp(argv[1], "maintenance") && !strcmp(argv[2], "--socket")) return holdMaintenanceLease(@(argv[3]));
        if (argc == 4 && !strcmp(argv[1], "check-path") && !strcmp(argv[2], "--path")) {
            NSString *error = nil;
            BOOL ok = safeRootPath(@(argv[3]), &error);
            emit(ok ? @{@"ok": @YES} : @{@"ok": @NO, @"error": error}); return ok ? 0 : 77;
        }
        if (argc < 4 || strcmp(argv[2], "--config")) {
            fprintf(stderr, "usage: farrow-mac-network {validate-config|probe|serve} --config FILE [--socket PATH]\n"); return 64;
        }
        NSString *command = @(argv[1]), *path = @(argv[3]);
        BOOL serving = [command isEqualToString:@"serve"];
        BOOL administrative = [command isEqualToString:@"admin-lock"];
        BOOL repairing = [command isEqualToString:@"repair-lock"] || administrative;
        if ((!serving && !repairing && argc != 4) || ((serving || repairing) && (argc != 6 || strcmp(argv[4], "--socket")))) return 64;
        if (![command isEqualToString:@"validate-config"] && ![command isEqualToString:@"probe"] && !serving && !repairing) return 64;
        NSString *error = nil;
        NetworkConfig *config = loadConfig(path, serving || repairing, &error);
        if (!config) { emit(@{@"ok": @NO, @"error": error ?: @"Invalid config"}); return 65; }
        if ([command isEqualToString:@"validate-config"]) {
            emit(@{@"ok": @YES, @"uid": @(config.uid), @"installation_id": config.installationID,
                   @"socket": socketPath(config), @"label": serviceLabel(config)}); return 0;
        }
        networkQueue = dispatch_queue_create("io.pgsty.farrow.mac-network", DISPATCH_QUEUE_SERIAL);
        if ([command isEqualToString:@"probe"]) return probe(config);
        if (geteuid() != 0 || !safeRootPath(@(argv[0]), &error)) {
            emit(@{@"ok": @NO, @"error": error ?: @"The network helper must run as root"}); return 77;
        }
        if (repairing) return holdRepairLock(config, @(argv[5]), administrative);
        return serve(config, @(argv[5]));
    }
}
