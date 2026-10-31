/* <sys/wait.h> for wasi-libc, which has no child processes. */
#ifndef WASI_COMPAT_SYS_WAIT_H
#define WASI_COMPAT_SYS_WAIT_H

#include <sys/types.h>

#define WNOHANG   1
#define WUNTRACED 2

#define WEXITSTATUS(s) (((s) & 0xff00) >> 8)
#define WTERMSIG(s)    ((s) & 0x7f)
#define WIFEXITED(s)   (WTERMSIG(s) == 0)
#define WIFSIGNALED(s) (((signed char) (((s) & 0x7f) + 1) >> 1) > 0)
#define WIFSTOPPED(s)  (((s) & 0xff) == 0x7f)
#define WSTOPSIG(s)    WEXITSTATUS(s)
#define WCOREDUMP(s)   ((s) & 0x80)

pid_t wait(int *status);
pid_t waitpid(pid_t pid, int *status, int options);

#endif
