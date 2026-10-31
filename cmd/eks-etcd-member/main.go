// eks-etcd-member removes a dead peer from the embedded etcd cluster of an EKS
// K3s control-plane VM. A member that is already gone exits 0; any other
// failure exits non-zero.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/etcdmember"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "remove" {
		fmt.Fprintln(stderr, "usage: eks-etcd-member remove --peer-ip 10.0.0.12 [--self-ip 10.0.0.10]")
		return 2
	}
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg etcdmember.Config
	endpoint := fs.String("endpoint", etcdmember.DefaultEndpoint, "etcd client endpoint")
	certDir := fs.String("cert-dir", etcdmember.DefaultCertDir, "directory holding the K3s etcd client.crt, client.key and server-ca.crt")
	timeout := fs.Duration("timeout", 30*time.Second, "overall deadline")
	fs.StringVar(&cfg.PeerIP, "peer-ip", "", "peer URL host of the member to remove")
	fs.StringVar(&cfg.SelfIP, "self-ip", "", "this host's own address; removal of it is refused")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cli, err := etcdmember.Dial(*endpoint, *certDir, *timeout)
	if err != nil {
		fmt.Fprintln(stderr, "etcd member remove failed:", err)
		return 1
	}
	defer func() { _ = cli.Close() }()
	return etcdmember.Run(ctx, cli, cfg, stdout)
}
