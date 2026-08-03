package handlers_bedrock

import (
	"fmt"
	"strings"
)

// servingAgentEnvPath is where the vllm-serving AMI's baked systemd unit
// reads its launch parameters from, mirroring RDS's agentEnvPath convention.
// The unit itself (and this path) lives in the AMI build, out of this bead's
// scope — this is the contract the launcher writes to it.
const servingAgentEnvPath = "/etc/conf.d/vllm-serve"

type serveUserDataInput struct {
	ModelID       string
	VLLMArgs      []string
	WeightsDevice string
	ServePort     int
}

// buildServeUserData renders the cloud-config write_files blob that hands the
// vllm-serving guest its model ID, extra vLLM args and the device its cloned
// weights volume landed on, following buildAgentUserData's write_files shape.
func buildServeUserData(in serveUserDataInput) string {
	body := strings.Join([]string{
		"VLLM_MODEL_ID=" + in.ModelID,
		"VLLM_ARGS=" + strings.Join(in.VLLMArgs, " "),
		"VLLM_WEIGHTS_DEVICE=" + in.WeightsDevice,
		fmt.Sprintf("VLLM_SERVE_PORT=%d", in.ServePort),
	}, "\n")

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")
	buf.WriteString("write_files:\n")
	fmt.Fprintf(&buf, "  - path: %s\n", servingAgentEnvPath)
	buf.WriteString("    permissions: '0600'\n")
	buf.WriteString("    content: |\n")
	for line := range strings.SplitSeq(body, "\n") {
		buf.WriteString("      ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return buf.String()
}
