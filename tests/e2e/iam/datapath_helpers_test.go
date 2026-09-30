//go:build e2e

package iam

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// sshHealth is this suite's sticky SSH-datapath verdict; see harness.SSHHealth.
var sshHealth harness.SSHHealth

// primaryENI returns the NetworkInterfaceId of an instance's first ENI.
// t.Fatal if the instance has no ENI — every running EC2 instance must.
func primaryENI(t *testing.T, inst *ec2.Instance) string {
	t.Helper()
	if len(inst.NetworkInterfaces) == 0 {
		t.Fatalf("instance %s has no NetworkInterfaces", aws.StringValue(inst.InstanceId))
	}
	eni := aws.StringValue(inst.NetworkInterfaces[0].NetworkInterfaceId)
	if eni == "" {
		t.Fatalf("instance %s primary ENI has empty NetworkInterfaceId", aws.StringValue(inst.InstanceId))
	}
	return eni
}

// detectPoolMode reads external_mode from spinifex.toml. Defaults to false
// (dev_networking) which is the single-node CI fixture. Any non-empty
// external_mode value ("pool" / "nat") means external IPAM is in play, which
// gates the IMDS probe-VPC public-IP path.
func detectPoolMode(env *harness.Env) bool {
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	f, err := os.Open(cfg)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inNetwork := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inNetwork = line == "[network]"
			continue
		}
		if !inNetwork {
			continue
		}
		if !strings.HasPrefix(line, "external_mode") {
			continue
		}
		// external_mode = "pool" — quoted value, anything non-empty == pool mode.
		if i := bytes.IndexByte([]byte(line), '='); i >= 0 {
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, "\"'")
			return val != ""
		}
	}
	return false
}
