/*
 * Force-included into every PostgreSQL translation unit (clang -include).
 *
 * wasi-libc has no processes, users, signals or sockets. PGlite's WASI port
 * was developed against pygame-web's python-wasi-sdk, whose compiler wrapper
 * injected a similar "hotfix" header. This file replaces that with the
 * declarations the PostgreSQL sources need; the definitions are in
 * wasi_compat.c, which is linked into the final module. Every stub that
 * stands in for a missing facility fails visibly (returns an error with
 * errno set) rather than pretending to succeed, except where PostgreSQL's
 * single-user mode needs a benign answer (process ids, umask).
 */
#ifndef WASI_COMPAT_H
#define WASI_COMPAT_H

#include <errno.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/types.h>

#ifdef __cplusplus
extern "C" {
#endif

/* WASI's errno set has no EHOSTDOWN; PostgreSQL lists it among the
 * connection-failure errnos. Any value outside WASI's range (which ends at
 * ENOTCAPABLE, 76) works. */
#ifndef EHOSTDOWN
#define EHOSTDOWN 117
#endif

/* <stdio.h>: there is no tmp directory convention on WASI. */
#ifndef P_tmpdir
#define P_tmpdir "/tmp"
#endif

/* <sys/file.h> flock() flags used by src/port and initdb. */
#define LOCK_SH 1
#define LOCK_EX 2
#define LOCK_NB 4
#define LOCK_UN 8

/* WASI files have no permission bits. wasi-libc's chmod/fchmod return
 * ENOSYS, which makes initdb refuse to continue; route the calls to no-ops
 * that succeed. (Overriding the symbols is not possible: wasi-libc bundles
 * them with other stubs in one object.) */
#define chmod wasi_compat_chmod
#define fchmod wasi_compat_fchmod
int wasi_compat_chmod(const char *path, mode_t mode);
int wasi_compat_fchmod(int fd, mode_t mode);

/* Processes. wasi-libc provides getpid via libwasi-emulated-getpid. */
pid_t getppid(void);
uid_t geteuid(void);
gid_t getegid(void);
mode_t umask(mode_t mask);
int kill(pid_t pid, int sig);
FILE *popen(const char *command, const char *type);
int pclose(FILE *stream);
int lockf(int fd, int cmd, off_t len);
char *mktemp(char *template);
char *tempnam(const char *dir, const char *pfx);

/* Sockets: declared by wasi-libc's <sys/socket.h> but not all defined for
 * wasip1. PostgreSQL's single-user backend never opens a listening socket;
 * these only have to link. */
int socket(int domain, int type, int protocol);
int bind(int sockfd, const void *addr, uint32_t addrlen);
int listen(int sockfd, int backlog);
int connect(int sockfd, const void *addr, uint32_t addrlen);
int setsockopt(int sockfd, int level, int optname, const void *optval, uint32_t optlen);
int getsockname(int sockfd, void *addr, uint32_t *addrlen);
int getpeername(int sockfd, void *addr, uint32_t *addrlen);
ssize_t sendto(int sockfd, const void *buf, size_t len, int flags, const void *dest_addr, uint32_t addrlen);
ssize_t recvfrom(int sockfd, void *buf, size_t len, int flags, void *src_addr, uint32_t *addrlen);
#ifndef SO_KEEPALIVE
#define SO_KEEPALIVE 9
#endif
#ifndef SO_REUSEADDR
#define SO_REUSEADDR 2
#endif

#ifdef __cplusplus
}
#endif

#endif /* WASI_COMPAT_H */
