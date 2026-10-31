/* <grp.h> for wasi-libc, which has no group database. Lookups fail. */
#ifndef WASI_COMPAT_GRP_H
#define WASI_COMPAT_GRP_H

#include <sys/types.h>

struct group {
	char *gr_name;
	char *gr_passwd;
	gid_t gr_gid;
	char **gr_mem;
};

struct group *getgrnam(const char *name);
struct group *getgrgid(gid_t gid);

#endif
