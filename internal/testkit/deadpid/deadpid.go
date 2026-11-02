// Package deadpid gives tests a PID that no process can hold.
package deadpid

// PID is above the kernel's PID_MAX_LIMIT (4194304), so kill(2) on it always
// fails with ESRCH. A plausible-looking literal such as 99999 is a real PID on
// a busy host, and signalling it terminates whatever process holds it.
const PID = 1 << 30
