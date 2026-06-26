package handlers_iam

import "encoding/json"

const (
	AccessKeyStatusActive   = "Active"
	AccessKeyStatusInactive = "Inactive"

	AccountStatusActive    = "ACTIVE"
	AccountStatusSuspended = "SUSPENDED"

	PolicyEffectAllow = "Allow"
	PolicyEffectDeny  = "Deny"
)

// User represents an IAM user stored in JetStream KV.
type User struct {
	UserName         string   `json:"user_name"`
	UserID           string   `json:"user_id"`
	AccountID        string   `json:"account_id"`
	ARN              string   `json:"arn"`
	Path             string   `json:"path"`
	CreatedAt        string   `json:"created_at"`
	AccessKeys       []string `json:"access_keys"`
	Tags             []Tag    `json:"tags"`
	AttachedPolicies []string `json:"attached_policies"` // policy ARNs
}

// AccessKey represents an IAM access key stored in JetStream KV.
// SecretAccessKey is AES-256-GCM encrypted (base64-encoded), not hashed,
// so the SigV4 middleware can recover the plaintext for signature verification.
type AccessKey struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"` // AES-256-GCM encrypted, base64-encoded
	UserName        string `json:"user_name"`
	AccountID       string `json:"account_id"`
	Status          string `json:"status"` // Active or Inactive
	CreatedAt       string `json:"created_at"`
}

// Tag represents a key-value tag on an IAM resource.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Policy represents a managed IAM policy stored in JetStream KV.
type Policy struct {
	PolicyName     string `json:"policy_name"`
	PolicyID       string `json:"policy_id"`
	ARN            string `json:"arn"`
	Path           string `json:"path"`
	Description    string `json:"description,omitempty"`
	PolicyDocument string `json:"policy_document"` // JSON string
	CreatedAt      string `json:"created_at"`
	DefaultVersion string `json:"default_version"` // always "v1"
	Tags           []Tag  `json:"tags"`
}

// PolicyDocument is the parsed IAM policy JSON structure.
type PolicyDocument struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is a single statement within a policy document.
type Statement struct {
	Sid      string      `json:"Sid,omitempty"`
	Effect   string      `json:"Effect"`
	Action   StringOrArr `json:"Action"`
	Resource StringOrArr `json:"Resource"`
}

// Role is an assumable IAM identity stored in JetStream KV.
// AssumeRolePolicyDocument is stored opaque; parsed for shape validation only.
type Role struct {
	RoleName                 string            `json:"role_name"`
	RoleID                   string            `json:"role_id"`
	AccountID                string            `json:"account_id"`
	ARN                      string            `json:"arn"`
	Path                     string            `json:"path"`
	Description              string            `json:"description,omitempty"`
	AssumeRolePolicyDocument string            `json:"assume_role_policy_document"`
	MaxSessionDuration       int64             `json:"max_session_duration,omitempty"`
	CreatedAt                string            `json:"created_at"`
	AttachedPolicies         []string          `json:"attached_policies"`         // policy ARNs
	InlinePolicies           map[string]string `json:"inline_policies,omitempty"` // policyName → document JSON
	Tags                     []Tag             `json:"tags"`
}

// InstanceProfile is a container for at most one Role, attachable to EC2 instances.
// AWS limits a profile to exactly one role; AddRoleToInstanceProfile enforces that.
type InstanceProfile struct {
	InstanceProfileName string `json:"instance_profile_name"`
	InstanceProfileID   string `json:"instance_profile_id"`
	AccountID           string `json:"account_id"`
	ARN                 string `json:"arn"`
	Path                string `json:"path"`
	RoleName            string `json:"role_name,omitempty"` // empty until AddRoleToInstanceProfile
	CreatedAt           string `json:"created_at"`
	Tags                []Tag  `json:"tags"`
}

// TrustPolicyDocument is the parsed AssumeRolePolicyDocument shape used for
// validation in CreateRole / UpdateAssumeRolePolicy. Principal and Condition
// are kept as RawMessage because trust-policy evaluation is deferred to STS.
type TrustPolicyDocument struct {
	Version   string           `json:"Version"`
	Statement []TrustStatement `json:"Statement"`
}

// TrustStatement is a single statement within a trust policy document.
// NotPrincipal and NotAction are present so the validator can detect and reject them;
// without them a policy with NotPrincipal would silently unmarshal as an empty Principal.
type TrustStatement struct {
	Sid          string          `json:"Sid,omitempty"`
	Effect       string          `json:"Effect"`
	Principal    json.RawMessage `json:"Principal"`
	NotPrincipal json.RawMessage `json:"NotPrincipal,omitempty"`
	Action       StringOrArr     `json:"Action"`
	NotAction    StringOrArr     `json:"NotAction,omitempty"`
	Condition    json.RawMessage `json:"Condition,omitempty"`
}

// StringOrArr handles JSON fields that can be either a string or an array of strings.
type StringOrArr []string

// UnmarshalJSON implements custom unmarshaling for string-or-array fields.
func (s *StringOrArr) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}

	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	*s = arr
	return nil
}

// MarshalJSON marshals as a string if single element, otherwise as an array.
func (s StringOrArr) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

// BootstrapData is the on-disk JSON file consumed on first gateway start
// to seed the root IAM user into NATS KV.
type BootstrapData struct {
	Version         string              `json:"version"`
	AccessKeyID     string              `json:"access_key_id"`
	EncryptedSecret string              `json:"encrypted_secret"`
	AccountID       string              `json:"account_id"`
	Admin           *AdminBootstrapData `json:"admin,omitempty"`
}

// AdminBootstrapData holds the default admin account credentials seeded
// alongside the system account during first boot.
type AdminBootstrapData struct {
	AccountID       string `json:"account_id"`
	AccountName     string `json:"account_name"`
	UserName        string `json:"user_name"`
	AccessKeyID     string `json:"access_key_id"`
	EncryptedSecret string `json:"encrypted_secret"`
}

// Account represents a Spinifex account. Accounts namespace IAM users,
// policies, and resources. Created via CLI only in v1.
type Account struct {
	AccountID   string `json:"account_id"`   // 12-digit zero-padded, sequential
	AccountName string `json:"account_name"` // Friendly name
	Status      string `json:"status"`       // "ACTIVE" or "SUSPENDED"
	CreatedAt   string `json:"created_at"`   // RFC3339
}
