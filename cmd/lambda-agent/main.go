// lambda-agent runs inside a Lambda guest.
//
// A normal build reads its per-generation identity from fw_cfg, unloads
// fw_cfg so nothing started later can read the guest key, and opens the
// authenticated control connection to the host. It accepts no invocations and
// never starts the function's bootstrap: the message protocol and grant
// fencing do not exist yet. The agent never listens for the host.
//
// A lambdatestlink build opens the same connection, then relays one invocation
// over a test carrier for local proofs; the carrier is not the guest protocol.
package main

import (
	"context"
	"flag"
	"os"
)

func main() { os.Exit(run()) }

func run() int {
	flag.Parse()
	return start(context.Background())
}
