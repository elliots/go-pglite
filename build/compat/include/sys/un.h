/* <sys/un.h> with a sun_path member. wasi-libc's sockaddr_un has none (WASI
 * has no Unix-domain sockets); PostgreSQL only needs the type to compile the
 * socket path checks in pqcomm.c, which the single-user backend never runs. */
#ifndef WASI_COMPAT_SYS_UN_H
#define WASI_COMPAT_SYS_UN_H

#include <sys/socket.h>

struct sockaddr_un {
	sa_family_t sun_family;
	char sun_path[108];
};

#endif
