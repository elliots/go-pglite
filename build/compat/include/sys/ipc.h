/* <sys/ipc.h> for wasi-libc. System V IPC does not exist on WASI; the
 * declarations let PostgreSQL's sysv_shmem.c compile. The implementations
 * in wasm_common.h (PG_SHMEM) hand out one malloc'd segment. */
#ifndef WASI_COMPAT_SYS_IPC_H
#define WASI_COMPAT_SYS_IPC_H

#include <sys/types.h>

struct ipc_perm {
	key_t __key;
	uid_t uid;
	gid_t gid;
	uid_t cuid;
	gid_t cgid;
	mode_t mode;
	unsigned short __seq;
};

#define IPC_CREAT  01000
#define IPC_EXCL   02000
#define IPC_NOWAIT 04000
#define IPC_RMID   0
#define IPC_SET    1
#define IPC_STAT   2
#define IPC_PRIVATE ((key_t) 0)

key_t ftok(const char *path, int id);

#endif
