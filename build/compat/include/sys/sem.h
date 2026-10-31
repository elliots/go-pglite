/* <sys/sem.h> for wasi-libc. System V semaphores do not exist on WASI;
 * PostgreSQL is configured for unnamed POSIX semaphores, so these only
 * satisfy the declarations in PGlite's sdk_port.h. */
#ifndef WASI_COMPAT_SYS_SEM_H
#define WASI_COMPAT_SYS_SEM_H

#include <sys/ipc.h>
#include <sys/types.h>

struct sembuf {
	unsigned short sem_num;
	short sem_op;
	short sem_flg;
};

#define SEM_UNDO 0x1000
#define GETVAL   12
#define SETVAL   16

#endif
