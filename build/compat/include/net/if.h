/* <net/if.h> for wasi-libc, which has no network interfaces. Deliberately
 * empty: with SIOCGIFCONF undefined, PostgreSQL's ifaddr.c falls back to
 * reporting only the loopback address. */
#ifndef WASI_COMPAT_NET_IF_H
#define WASI_COMPAT_NET_IF_H
#endif
