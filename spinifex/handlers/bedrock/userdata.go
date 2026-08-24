package handlers_bedrock

import (
	"fmt"
	"strings"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// bundleAgentEnvDir is where each bundle member's env file is written, one
// per co-served model, so the AMI's per-member systemd unit instances (one
// vllm-serve@ or tei-serve@ instance per member) each source their own
// launch parameters without colliding. A standalone model writes exactly one
// file here — the bundle-of-one case, same directory either way.
const bundleAgentEnvDir = "/etc/conf.d/bedrock-bundle"

// memberEnvPath returns the env file path a bundle member's systemd unit
// instance reads from. A model id's ':' and '.' are not valid systemd
// instance-name characters, so the id is folded to a safe one first.
func memberEnvPath(modelID string) string {
	return bundleAgentEnvDir + "/" + sanitizeMemberInstanceName(modelID) + ".env"
}

var memberInstanceNameReplacer = strings.NewReplacer(":", "_", "/", "_", ".", "_")

func sanitizeMemberInstanceName(modelID string) string {
	return memberInstanceNameReplacer.Replace(modelID)
}

// engineForFamily selects which serving engine a family runs under: familyMeta
// always runs vLLM, every other family (familyTEI included) runs TEI. Mirrors
// the invoke-side dispatch in gateway_bedrock, so a family unknown there is
// unknown here too rather than silently defaulting to one engine.
func engineForFamily(family string) string {
	if family == gateway_bedrock.FamilyMeta {
		return "vllm"
	}
	return "tei"
}

// bundleMemberUserData is one member's own write_files entry within a
// bundle's userData: its engine, model id, extra engine args, the device its
// cloned weights volume landed on, and the port it serves on.
type bundleMemberUserData struct {
	ModelID       string
	Family        string
	VLLMArgs      []string
	WeightsDevice string
	Port          int
}

// bundleUserDataInput is every member of one bundle's shared VM.
type bundleUserDataInput struct {
	GroupID string
	Members []bundleMemberUserData
}

// buildBundleUserData renders the cloud-config write_files blob that hands
// the vllm/tei-serving AMI each member's own launch parameters: which engine
// to run it under, its model id, extra engine args, the device its cloned
// weights volume landed on, and the port to serve on. A standalone model is
// the one-member case of this same shape, so this is the only userData
// builder the launcher needs.
func buildBundleUserData(in bundleUserDataInput) string {
	var buf strings.Builder
	buf.WriteString("#cloud-config\n")
	buf.WriteString("write_files:\n")
	for _, m := range in.Members {
		body := strings.Join([]string{
			"BEDROCK_GROUP_ID=" + in.GroupID,
			"BEDROCK_ENGINE=" + engineForFamily(m.Family),
			"BEDROCK_MODEL_ID=" + m.ModelID,
			"BEDROCK_ARGS=" + strings.Join(m.VLLMArgs, " "),
			"BEDROCK_WEIGHTS_DEVICE=" + m.WeightsDevice,
			fmt.Sprintf("BEDROCK_SERVE_PORT=%d", m.Port),
		}, "\n")

		fmt.Fprintf(&buf, "  - path: %s\n", memberEnvPath(m.ModelID))
		buf.WriteString("    permissions: '0600'\n")
		buf.WriteString("    content: |\n")
		for line := range strings.SplitSeq(body, "\n") {
			buf.WriteString("      ")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	return buf.String()
}
