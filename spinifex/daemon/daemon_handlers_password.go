package daemon

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"golang.org/x/crypto/ssh"
)

// passwordDataPattern matches a <Password>...</Password> block. The
// character class excludes '<' so an unterminated opener (no matching close
// tag before the next '<') simply fails to match rather than swallowing
// unrelated console output that follows it.
var passwordDataPattern = regexp.MustCompile(`<Password>([^<]*)</Password>`)

// extractLastPasswordData scans console log data for <Password>...</Password>
// blocks and returns the base64 ciphertext of the last one, matching AWS's
// "last such data emitted" semantics. CRLF line endings and surrounding
// whitespace from interleaved boot output are trimmed. Returns "" when no
// well-formed block is present.
func extractLastPasswordData(data []byte) string {
	matches := passwordDataPattern.FindAllSubmatch(data, -1)
	if len(matches) == 0 {
		return ""
	}
	last := matches[len(matches)-1][1]
	return strings.TrimSpace(string(last))
}

// launchKeyCannotEncrypt reports whether the instance's launch key pair is
// incapable of wrapping a password. Only Windows instances are judged: no other
// platform emits one, so a Linux instance keeps AWS's empty result.
//
// Every uncertainty answers false. A key that cannot be resolved, or a platform
// that was never stamped, is not evidence of the wrong key type, and turning a
// lookup blip into a terminal error would be worse than the empty string.
func (d *Daemon) launchKeyCannotEncrypt(instance *vm.VM) bool {
	if instance.Instance == nil || aws.StringValue(instance.Instance.Platform) != utils.PlatformWindows {
		return false
	}
	keyName := aws.StringValue(instance.Instance.KeyName)
	if keyName == "" || d.keyService == nil {
		return false
	}
	material, err := d.keyService.GetPublicKeyMaterial(instance.AccountID, keyName)
	if err != nil {
		slog.Warn("GetPasswordData: could not resolve launch key type", "instance_id", instance.ID, "key_name", keyName, "err", err)
		return false
	}
	// The OpenSSH algorithm prefix is the key type; ssh-rsa is the only one that
	// can do the PKCS#1 v1.5 encryption the guest agent performs.
	return material != "" && !strings.HasPrefix(material, ssh.KeyAlgoRSA+" ")
}

// handleEC2GetPasswordData reads the console log file for an instance and
// returns the last password blob the guest emitted, matching the AWS
// GetPasswordData API response format. The blob is opaque ciphertext
// encrypted with the launch key pair's public key: Spinifex never handles
// key material and never sees the plaintext password.
func (d *Daemon) handleEC2GetPasswordData(msg *nats.Msg) {
	slog.Debug("Received GetPasswordData request", "subject", msg.Subject, "data", string(msg.Data))

	var input ec2.GetPasswordDataInput
	if errResp := utils.UnmarshalJsonPayload(&input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}

	if input.InstanceId == nil {
		respondWithError(d.node, msg, awserrors.ErrorMissingParameter)
		return
	}

	instanceID := *input.InstanceId

	// Find the instance on this node
	instance, exists := d.vmMgr.Get(instanceID)
	if !exists {
		respondWithError(d.node, msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	// Verify the caller owns this instance
	if !checkInstanceOwnership(d.node, msg, instanceID, instance.AccountID) {
		return
	}

	logPath := instance.Config.ConsoleLogPath
	var passwordData string
	var modTime time.Time

	if logPath != "" {
		info, err := os.Stat(logPath)
		if err == nil {
			modTime = info.ModTime()

			// Scan the whole log, not just the AWS 64KB console tail: the
			// password is emitted once at first boot and could scroll off
			// a long-running guest before it is ever fetched.
			data, err := os.ReadFile(logPath)
			if err != nil {
				slog.Error("Failed to read console log", "path", logPath, "err", err)
			} else {
				passwordData = extractLastPasswordData(data)
			}
		}
	}

	// An empty result is ordinarily "the guest has not emitted one yet", which is
	// worth retrying. A non-RSA launch key never will, so name that case instead
	// of returning the same empty string for a condition that cannot resolve.
	if passwordData == "" && d.launchKeyCannotEncrypt(instance) {
		respondWithError(d.node, msg, awserrors.ErrorInvalidKeyPairType)
		return
	}

	now := time.Now()
	if modTime.IsZero() {
		modTime = now
	}

	output := &ec2.GetPasswordDataOutput{
		InstanceId:   &instanceID,
		PasswordData: &passwordData,
		Timestamp:    &modTime,
	}

	respondWithJSON(d.node, msg, output)
	// Log the ciphertext length only; the payload itself is never logged.
	slog.Info("handleEC2GetPasswordData completed", "instance_id", instanceID, "password_data_bytes", len(passwordData))
}
