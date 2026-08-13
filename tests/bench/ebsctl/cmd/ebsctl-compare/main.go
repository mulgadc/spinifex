// Command ebsctl-compare reads two ebsctl-bench result JSON files and emits
// a markdown comparison table (medians, p95s, deltas, sample counts, error
// rates, and a bootstrap-CI significance signal per operation). No cluster
// or build tag required — it only consumes the JSON schema, so it builds
// and runs anywhere `go build` does.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mulgadc/spinifex/tests/bench/ebsctl"
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s [-out path] <a.json> <b.json>\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	out := flag.String("out", "", "write markdown to this path instead of stdout")
	flag.Parse()

	if flag.NArg() != 2 {
		usage()
		os.Exit(2)
	}

	a, err := ebsctl.LoadResult(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ebsctl-compare: %v\n", err)
		os.Exit(1)
	}
	b, err := ebsctl.LoadResult(flag.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ebsctl-compare: %v\n", err)
		os.Exit(1)
	}

	md := ebsctl.CompareRuns(a, b, time.Now().UnixNano())

	if *out == "" {
		fmt.Print(md)
		return
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ebsctl-compare: write %s: %v\n", *out, err)
		os.Exit(1)
	}
}
