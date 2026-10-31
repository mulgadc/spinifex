// eks-etcd-member removes a dead peer from the embedded etcd cluster of an EKS
// K3s control-plane VM. A member that is already gone exits 0; any other
// failure exits non-zero.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/etcdmember"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "remove" {
		fmt.Fprintln(os.Stderr, "usage: eks-etcd-member remove --peer-ip 10.0.0.12 [--self-ip 10.0.0.10]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	var cfg etcdmember.Config
	endpoint := fs.String("endpoint", etcdmember.DefaultEndpoint, "etcd client endpoint")
	certDir := fs.String("cert-dir", etcdmember.DefaultCertDir, "directory holding the K3s etcd client.crt, client.key and server-ca.crt")
	timeout := fs.Duration("timeout", 30*time.Second, "overall deadline")
	fs.StringVar(&cfg.PeerIP, "peer-ip", "", "peer URL host of the member to remove")
	fs.StringVar(&cfg.SelfIP, "self-ip", "", "this host's own address; removal of it is refused")
	_ = fs.Parse(os.Args[2:])

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cli, err := etcdmember.Dial(*endpoint, *certDir, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "etcd member remove failed:", err)
		os.Exit(1)
	}
	code := etcdmember.Run(ctx, cli, cfg, os.Stdout)
	_ = cli.Close()
	os.Exit(code)
}
