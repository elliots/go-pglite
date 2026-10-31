/*
 * Definitions for wasi_compat.h and the compat headers. Linked once into
 * pglite.wasi. Each function stands in for a host facility WASI does not
 * have. Where PostgreSQL only needs a plausible answer in single-user mode
 * (process ids, umask) one is returned; everything else fails with errno
 * set so that a code path that unexpectedly reaches it reports an error
 * instead of silently continuing.
 */
#include <dlfcn.h>
#include <errno.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "grp.h"
#include "netdb.h"
#include "pwd.h"
#include "sys/wait.h"
#include "wasi_compat.h"

pid_t getppid(void) { return 1; }
uid_t geteuid(void) { return 1000; }
gid_t getegid(void) { return 1000; }
mode_t umask(mode_t mask) { (void) mask; return 022; }

/* WASI files have no permission bits, so there is nothing to change (see
 * the chmod macro in wasi_compat.h). */
int wasi_compat_chmod(const char *path, mode_t mode) { (void) path; (void) mode; return 0; }
int wasi_compat_fchmod(int fd, mode_t mode) { (void) fd; (void) mode; return 0; }

int kill(pid_t pid, int sig)
{
	(void) pid;
	(void) sig;
	errno = ENOSYS;
	return -1;
}

FILE *popen(const char *command, const char *type)
{
	(void) command;
	(void) type;
	errno = ENOSYS;
	return NULL;
}

int pclose(FILE *stream)
{
	(void) stream;
	errno = ENOSYS;
	return -1;
}

int lockf(int fd, int cmd, off_t len)
{
	(void) fd;
	(void) cmd;
	(void) len;
	errno = ENOSYS;
	return -1;
}

char *mktemp(char *template)
{
	errno = ENOSYS;
	if (template)
		*template = '\0';
	return template;
}

char *tempnam(const char *dir, const char *pfx)
{
	(void) dir;
	(void) pfx;
	errno = ENOSYS;
	return NULL;
}

/* dlopen/dlsym/dlerror resolve from a table in PGlite's
 * sdk_port-wasi-dlfcn.c; a handle from that table has nothing to release. */
int dlclose(void *handle) { (void) handle; return 0; }

/* waitpid() is defined by PGlite's sdk_port-wasi.c. */
pid_t wait(int *status) { (void) status; errno = ECHILD; return -1; }

struct passwd *getpwnam(const char *name) { (void) name; errno = ENOENT; return NULL; }
struct passwd *getpwuid(uid_t uid) { (void) uid; errno = ENOENT; return NULL; }
int getpwuid_r(uid_t uid, struct passwd *pwd, char *buf, size_t buflen, struct passwd **result)
{
	(void) uid;
	(void) pwd;
	(void) buf;
	(void) buflen;
	*result = NULL;
	return ENOENT;
}
int getpwnam_r(const char *name, struct passwd *pwd, char *buf, size_t buflen, struct passwd **result)
{
	(void) name;
	(void) pwd;
	(void) buf;
	(void) buflen;
	*result = NULL;
	return ENOENT;
}
struct group *getgrnam(const char *name) { (void) name; errno = ENOENT; return NULL; }
struct group *getgrgid(gid_t gid) { (void) gid; errno = ENOENT; return NULL; }

/* Sockets: wasip1 has no socket creation. */
int socket(int domain, int type, int protocol)
{
	(void) domain;
	(void) type;
	(void) protocol;
	errno = ENOSYS;
	return -1;
}
int bind(int sockfd, const void *addr, uint32_t addrlen)
{
	(void) sockfd;
	(void) addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}
int listen(int sockfd, int backlog) { (void) sockfd; (void) backlog; errno = ENOSYS; return -1; }
int connect(int sockfd, const void *addr, uint32_t addrlen)
{
	(void) sockfd;
	(void) addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}
int setsockopt(int sockfd, int level, int optname, const void *optval, uint32_t optlen)
{
	(void) sockfd;
	(void) level;
	(void) optname;
	(void) optval;
	(void) optlen;
	errno = ENOSYS;
	return -1;
}
int getsockname(int sockfd, void *addr, uint32_t *addrlen)
{
	(void) sockfd;
	(void) addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}
int getpeername(int sockfd, void *addr, uint32_t *addrlen)
{
	(void) sockfd;
	(void) addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}
ssize_t sendto(int sockfd, const void *buf, size_t len, int flags, const void *dest_addr, uint32_t addrlen)
{
	(void) sockfd;
	(void) buf;
	(void) len;
	(void) flags;
	(void) dest_addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}
ssize_t recvfrom(int sockfd, void *buf, size_t len, int flags, void *src_addr, uint32_t *addrlen)
{
	(void) sockfd;
	(void) buf;
	(void) len;
	(void) flags;
	(void) src_addr;
	(void) addrlen;
	errno = ENOSYS;
	return -1;
}

int getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **res)
{
	(void) node;
	(void) service;
	(void) hints;
	*res = NULL;
	return EAI_FAIL;
}
void freeaddrinfo(struct addrinfo *res) { (void) res; }
const char *gai_strerror(int errcode)
{
	(void) errcode;
	return "name resolution is not available on WASI";
}
int getnameinfo(const struct sockaddr *addr, socklen_t addrlen, char *host, socklen_t hostlen,
				char *serv, socklen_t servlen, int flags)
{
	(void) addr;
	(void) addrlen;
	(void) flags;
	if (host && hostlen)
		host[0] = '\0';
	if (serv && servlen)
		serv[0] = '\0';
	return EAI_FAIL;
}
struct hostent *gethostbyname(const char *name) { (void) name; return NULL; }
struct servent *getservbyname(const char *name, const char *proto) { (void) name; (void) proto; return NULL; }
struct protoent *getprotobyname(const char *name) { (void) name; return NULL; }
