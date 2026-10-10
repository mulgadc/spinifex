// ecr-credential-provider is the kubelet exec credential provider for Spinifex
// EKS worker nodes; the implementation is in agents/eks/credentialprovider.
package main

import (
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/credentialprovider"
)

func main() {
	os.Exit(credentialprovider.Main(os.Stdin, os.Stdout))
}
