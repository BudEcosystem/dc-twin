/*
 * vrfbind: `ip vrf exec <vrf>` for a container (18 — Z7a). Linux's ip vrf exec puts the command
 * in a cgroup whose BPF program binds every socket it opens to the VRF's device; a container can
 * make neither. Loaded with LD_PRELOAD and DCTWIN_VRF=<vrf> (dctwin-vrf-exec, the twin's ip), this
 * binds each IPv4 and IPv6 socket the program opens to the VRF's device with SO_BINDTODEVICE, so
 * its traffic is routed in the VRF's table, as under ip vrf exec. Netlink and packet sockets are
 * left alone (an AF_PACKET socket bound to the VRF device would see nothing of its slaves).
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int socket(int domain, int type, int protocol) {
	static int (*real)(int, int, int);
	if (!real)
		real = (int (*)(int, int, int))dlsym(RTLD_NEXT, "socket");
	int fd = real(domain, type, protocol);
	const char *vrf = getenv("DCTWIN_VRF");
	if (fd < 0 || !vrf || !*vrf || (domain != AF_INET && domain != AF_INET6))
		return fd;
	if (setsockopt(fd, SOL_SOCKET, SO_BINDTODEVICE, vrf, strlen(vrf) + 1) < 0) {
		/* a socket outside the VRF would leave through the default VRF: refuse it */
		int e = errno;
		close(fd);
		errno = e;
		return -1;
	}
	return fd;
}
