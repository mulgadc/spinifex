// Test-only backstop preloaded into QEMU: before main it arms a SIGKILL timer at an
// absolute CLOCK_BOOTTIME deadline, then acks on an inherited fd; any failure kills it.
// Build: cc -O2 -Wall -Wextra -shared -fPIC -o hardkill.so hardkill.c -lrt
#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

static void die(void) {
	kill(getpid(), SIGKILL);
	_exit(127);
}

static long parse(const char *s, long min, long max) {
	if (!s || !*s) die();
	char *end;
	errno = 0;
	long v = strtol(s, &end, 10);
	if (errno || *end || v < min || v > max) die();
	return v;
}

__attribute__((constructor)) static void hardkill_arm(void) {
	long dl = parse(getenv("SPX_HARDKILL_DEADLINE_NS"), 1, LONG_MAX);
	int ack = (int)parse(getenv("SPX_HARDKILL_ACK_FD"), 3, INT_MAX);
	int lib = (int)parse(getenv("SPX_HARDKILL_LIB_FD"), 3, INT_MAX);
	if (lib == ack) die();
	const char *nonce = getenv("SPX_HARDKILL_NONCE");
	char tag[33];
	if (!nonce || strlen(nonce) != 32 || strspn(nonce, "0123456789abcdef") != 32) die();
	memcpy(tag, nonce, sizeof tag);
	// Nothing QEMU starts later inherits the backstop's settings.
	if (unsetenv("SPX_HARDKILL_DEADLINE_NS") || unsetenv("SPX_HARDKILL_ACK_FD") ||
	    unsetenv("SPX_HARDKILL_NONCE") || unsetenv("SPX_HARDKILL_LIB_FD") ||
	    unsetenv("LD_PRELOAD")) die();

	struct sigevent sev;
	memset(&sev, 0, sizeof sev);
	sev.sigev_notify = SIGEV_SIGNAL;
	sev.sigev_signo = SIGKILL;
	timer_t t;
	if (timer_create(CLOCK_BOOTTIME, &sev, &t) != 0) die();
	struct itimerspec its;
	memset(&its, 0, sizeof its);
	its.it_value.tv_sec = dl / 1000000000L;
	its.it_value.tv_nsec = dl % 1000000000L;
	// A deadline already past makes the kill due at once.
	if (timer_settime(t, TIMER_ABSTIME, &its, NULL) != 0) die();

	// The loader has mapped the library; the ack also proves QEMU keeps no
	// handle on its memfd.
	if (close(lib) != 0) die();

	char msg[128];
	int n = snprintf(msg, sizeof msg, "armed %d %ld %s\n", getpid(), dl, tag);
	if (n <= 0 || n >= (int)sizeof msg) die();
	for (int off = 0; off < n;) {
		ssize_t w = write(ack, msg + off, (size_t)(n - off));
		if (w < 0 && errno == EINTR) continue;
		if (w <= 0) die();
		off += (int)w;
	}
	if (close(ack) != 0) die();
}
