//go:build lambdatestlink

// Command lambda-image-digest prints the test-link runtime image digest of one
// directory, selecting the boot files the same way the daemon does. The digest
// identifies the bytes; it does not authenticate where they came from.
package main

import (
	"fmt"
	"os"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: lambda-image-digest <image-dir>")
		os.Exit(2)
	}
	digest, err := testlink.DirDigest(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
