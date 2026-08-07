//go:build e2e && bench

package ebsctl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// A benchmark that mislabels which [ebs] provider produced its numbers is
// worse than no benchmark, so DetectEBSProvider reads the value off the
// cluster itself rather than trusting an operator-supplied flag by default.
//
// Single-node mode: the test binary runs on the same host as the config
// (mirrors harness's own detectPoolMode), so the file is read locally.
// Multinode/baremetal: the binary typically runs off-cluster, so every node
// in SPINIFEX_NODE_IPS is read over SSH (SPINIFEX_SSH_USER/SPINIFEX_SSH_KEY)
// and the values must agree — a split cluster is a genuine configuration
// problem, not something to paper over with the first answer found.
//
// -provider is the fallback: required and validated when detection fails,
// and cross-checked against a successful detection rather than silently
// overriding it.
func DetectEBSProvider(t *testing.T, env *harness.Env) (provider, source string) {
	t.Helper()

	var detected string
	var detErr error
	switch env.Mode {
	case harness.ModeSingle:
		detected, detErr = detectProviderLocal(env)
		source = "cluster-local-config"
	default:
		detected, detErr = detectProviderSSH(env)
		source = "cluster-ssh"
	}

	if detErr != nil {
		p := requireProviderFlag(t, detErr.Error())
		return p, "flag"
	}
	if *flagProvider != "" && *flagProvider != detected {
		t.Fatalf("-provider=%q conflicts with the cluster-detected value %q (source=%s); "+
			"fix the flag or omit it to trust detection", *flagProvider, detected, source)
	}
	return detected, source
}

func requireProviderFlag(t *testing.T, reason string) string {
	t.Helper()
	if *flagProvider == "" {
		t.Fatalf("cannot determine [ebs] provider from the cluster (%s); "+
			"pass -provider=%s explicitly", reason, config.EBSProviderViperblockd)
	}
	if *flagProvider != config.EBSProviderViperblockd {
		t.Fatalf("-provider=%q is not a recognised value (want %q)",
			*flagProvider, config.EBSProviderViperblockd)
	}
	t.Logf("WARNING: could not detect [ebs] provider from the cluster (%s); trusting -provider=%s",
		reason, *flagProvider)
	return *flagProvider
}

// detectProviderLocal reads spinifex.toml from the local filesystem, trying
// the same candidate paths as harness.LoadEnv/detectPoolMode.
func detectProviderLocal(env *harness.Env) (string, error) {
	candidates := []string{}
	if env.ConfigDir != "" {
		candidates = append(candidates, filepath.Join(env.ConfigDir, "spinifex.toml"))
	}
	candidates = append(candidates,
		"/etc/spinifex/spinifex.toml",
		os.ExpandEnv("$HOME/spinifex/config/spinifex.toml"),
	)

	var lastErr error
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		return resolvedProviderFromTOML(string(data))
	}
	return "", fmt.Errorf("no readable spinifex.toml among %v: %w", candidates, lastErr)
}

// detectProviderSSH reads spinifex.toml from every node in env.NodeIPs over
// SSH and requires unanimous agreement on the resolved provider value.
func detectProviderSSH(env *harness.Env) (string, error) {
	if len(env.NodeIPs) == 0 {
		return "", errors.New("no SPINIFEX_NODE_IPS to query")
	}

	peer := harness.NewPeerSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seen := map[string][]string{} // provider -> node IPs reporting it
	var details []string
	for _, ip := range env.NodeIPs {
		out, err := peer.Run(ctx, ip, "cat /etc/spinifex/spinifex.toml 2>/dev/null || cat $HOME/spinifex/config/spinifex.toml 2>/dev/null")
		if err != nil || strings.TrimSpace(string(out)) == "" {
			details = append(details, fmt.Sprintf("%s: unreachable or empty config (%v)", ip, err))
			continue
		}
		p, perr := resolvedProviderFromTOML(string(out))
		if perr != nil {
			details = append(details, fmt.Sprintf("%s: %v", ip, perr))
			continue
		}
		seen[p] = append(seen[p], ip)
		details = append(details, fmt.Sprintf("%s: %s", ip, p))
	}

	if len(seen) == 1 {
		for p := range seen {
			return p, nil
		}
	}
	return "", fmt.Errorf("%d/%d node(s) queried disagree or were unreachable: %s",
		len(details), len(env.NodeIPs), strings.Join(details, "; "))
}

var (
	tomlTopNodeLine  = regexp.MustCompile(`(?i)^\s*node\s*=\s*"([^"]+)"`)
	tomlProviderLine = regexp.MustCompile(`(?i)^\s*provider\s*=\s*"([^"]*)"`)
)

// resolvedProviderFromTOML extracts [nodes.<node>.ebs] provider from a
// spinifex.toml's contents, where <node> is the file's own top-level `node`
// key (the node's name for itself). An unset provider resolves to
// config.EBSProviderViperblockd via config.EBSConfig.ResolvedProvider, the same
// default the daemon itself applies — so detection never invents its own
// notion of "default".
func resolvedProviderFromTOML(content string) (string, error) {
	var node, provider string
	inEBS := false
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inEBS = node != "" && line == "[nodes."+node+".ebs]"
			continue
		}
		if node == "" {
			if m := tomlTopNodeLine.FindStringSubmatch(line); m != nil {
				node = m[1]
			}
			continue
		}
		if inEBS {
			if m := tomlProviderLine.FindStringSubmatch(line); m != nil {
				provider = m[1]
			}
		}
	}
	if node == "" {
		return "", errors.New("no top-level node key found in spinifex.toml")
	}
	return config.EBSConfig{Provider: provider}.ResolvedProvider(), nil
}
