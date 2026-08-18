package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/spf13/cobra"
)

// adminPrincipalPolicyName is the inline policy every admin principal carries.
// One name for every principal is what makes the listing readable: a user with
// this policy is an admin-API caller and its actions are the whole grant.
const adminPrincipalPolicyName = "spx-admin-methods"

// adminPrincipalService is the SigV4 credential scope the admin surface
// requires, and the prefix each granted action carries.
const adminPrincipalService = "spinifex"

// runAdminPrincipalCreate provisions a named principal in the super-admin
// account. Every step is create-if-absent except the access key, which is
// replaced so exactly one key is ever live.
func runAdminPrincipalCreate(cmd *cobra.Command, args []string) error {
	userName := args[0]
	if err := validatePrincipalName(userName); err != nil {
		return err
	}

	requested, _ := cmd.Flags().GetStringSlice("grant")
	grants, err := resolvePrincipalGrants(requested)
	if err != nil {
		return err
	}

	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()

	if _, err := svc.CreateUser(accountID, &iam.CreateUserInput{
		UserName: aws.String(userName),
	}); err != nil && !strings.Contains(err.Error(), "EntityAlreadyExists") {
		return fmt.Errorf("create user %s: %w", userName, err)
	}

	if _, err := svc.PutUserPolicy(accountID, &iam.PutUserPolicyInput{
		UserName:       aws.String(userName),
		PolicyName:     aws.String(adminPrincipalPolicyName),
		PolicyDocument: aws.String(principalPolicyDocument(grants)),
	}); err != nil {
		return fmt.Errorf("attach policy to %s: %w", userName, err)
	}

	dropped, err := dropStalePrincipalPolicies(svc, accountID, userName)
	if err != nil {
		return err
	}
	for _, name := range dropped {
		fmt.Printf("Removed stale inline policy %s\n", name)
	}

	revoked, err := revokePrincipalKeys(svc, accountID, userName)
	if err != nil {
		return err
	}
	for _, keyID := range revoked {
		fmt.Printf("Revoked previous access key %s\n", keyID)
	}

	akOut, err := svc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return fmt.Errorf("create access key for %s: %w", userName, err)
	}
	if akOut == nil || akOut.AccessKey == nil {
		return fmt.Errorf("create access key for %s: no key returned", userName)
	}

	printPrincipalCredential(accountID, userName, grants,
		aws.StringValue(akOut.AccessKey.AccessKeyId),
		aws.StringValue(akOut.AccessKey.SecretAccessKey))
	return nil
}

// printPrincipalCredential writes the one and only view of the secret, then the
// two things a caller still needs: the SigV4 scope and the region the key is
// useless without.
func printPrincipalCredential(accountID, userName string, grants []string, keyID, secret string) {
	fmt.Printf("\nPrincipal %q ready.\n", userName)
	fmt.Printf("  Account ID:        %s\n", accountID)
	fmt.Printf("  User:              %s\n", userName)
	fmt.Printf("  Permitted actions: %s\n", strings.Join(grants, ", "))
	fmt.Printf("  Access Key ID:     %s\n", keyID)
	fmt.Printf("  Secret Access Key: %s\n", secret)
	fmt.Println("\nThe secret is shown once and is not recoverable. Store it in a secret manager")
	fmt.Println("or an AWS profile; never commit it and never place it in a Spinifex config file.")
	fmt.Printf("\nRequests must be SigV4-signed with service=%q and this cluster's region.\n", adminPrincipalService)
}

// runAdminPrincipalList reports every principal in the super-admin account and
// what it may call, so an operator can answer "who can delete an account" from
// the cluster rather than from memory.
func runAdminPrincipalList(_ *cobra.Command, _ []string) error {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()
	users, err := svc.ListUsers(accountID, &iam.ListUsersInput{})
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	rows := make([]principalRow, 0, len(users.Users))
	for _, user := range users.Users {
		if user == nil || user.UserName == nil {
			continue
		}
		row, err := describePrincipal(svc, accountID, aws.StringValue(user.UserName))
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}

	printPrincipalTable(accountID, rows)
	return nil
}

// principalRow is one line of the principal listing.
type principalRow struct {
	UserName string
	Grants   []string
	Keys     int
}

// describePrincipal resolves what a user in the super-admin account may call.
// An attached wildcard policy is reported as such rather than expanded: the
// point of the listing is to show which principals are unscoped.
func describePrincipal(svc handlers_iam.IAMService, accountID, userName string) (principalRow, error) {
	row := principalRow{UserName: userName}

	attached, err := svc.ListAttachedUserPolicies(accountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return row, fmt.Errorf("list attached policies for %s: %w", userName, err)
	}
	if attached != nil {
		for _, policy := range attached.AttachedPolicies {
			if policy == nil {
				continue
			}
			row.Grants = append(row.Grants, aws.StringValue(policy.PolicyName)+" (attached)")
		}
	}

	inline, err := svc.GetUserPolicy(accountID, &iam.GetUserPolicyInput{
		UserName:   aws.String(userName),
		PolicyName: aws.String(adminPrincipalPolicyName),
	})
	if err == nil && inline != nil {
		row.Grants = append(row.Grants, principalPolicyActions(aws.StringValue(inline.PolicyDocument))...)
	}

	keys, err := svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{UserName: aws.String(userName)})
	if err != nil {
		return row, fmt.Errorf("list access keys for %s: %w", userName, err)
	}
	if keys != nil {
		row.Keys = len(keys.AccessKeyMetadata)
	}
	return row, nil
}

// principalPolicyActions returns the actions an inline policy allows, shortened
// to the method name. An undecodable document reports nothing rather than
// guessing, and the listing shows the user with no grants.
func principalPolicyActions(document string) []string {
	var doc struct {
		Statement []struct {
			Effect string                   `json:"Effect"`
			Action handlers_iam.StringOrArr `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		return nil
	}

	var actions []string
	for _, stmt := range doc.Statement {
		if stmt.Effect != "Allow" {
			continue
		}
		for _, action := range stmt.Action {
			actions = append(actions, strings.TrimPrefix(action, adminPrincipalService+":"))
		}
	}
	sort.Strings(actions)
	return actions
}

// printPrincipalTable renders the listing.
func printPrincipalTable(accountID string, rows []principalRow) {
	fmt.Printf("\nPrincipals in the super-admin account (%s):\n\n", accountID)
	fmt.Printf("%-24s %-6s %s\n", "USER", "KEYS", "GRANTS")
	fmt.Println(strings.Repeat("-", 96))
	for _, row := range rows {
		grants := strings.Join(row.Grants, ", ")
		if grants == "" {
			grants = "-"
		}
		fmt.Printf("%-24s %-6d %s\n", row.UserName, row.Keys, grants)
	}
	fmt.Printf("\n%d principal(s)\n", len(rows))
}

// runAdminPrincipalRevoke removes every access key a principal holds, leaving
// the user and its policy in place. This is the response to a leaked secret:
// it takes effect cluster-wide immediately and needs no restart.
func runAdminPrincipalRevoke(_ *cobra.Command, args []string) error {
	userName := args[0]

	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()
	if _, err := svc.GetUser(accountID, &iam.GetUserInput{UserName: aws.String(userName)}); err != nil {
		return fmt.Errorf("no principal %q in account %s", userName, accountID)
	}

	revoked, err := revokePrincipalKeys(svc, accountID, userName)
	if err != nil {
		return err
	}
	if len(revoked) == 0 {
		fmt.Printf("Principal %q holds no access keys.\n", userName)
		return nil
	}
	for _, keyID := range revoked {
		fmt.Printf("Revoked access key %s\n", keyID)
	}
	fmt.Printf("\nPrincipal %q can no longer sign requests. Re-issue with:\n", userName)
	fmt.Printf("  spx admin principal create %s\n", userName)
	return nil
}

// dropStalePrincipalPolicies removes every inline policy on the principal that
// this command did not write. A principal's grants are exactly one policy, so
// re-creating one with narrower grants cannot leave a wider policy behind.
func dropStalePrincipalPolicies(svc handlers_iam.IAMService, accountID, userName string) ([]string, error) {
	listed, err := svc.ListUserPolicies(accountID, &iam.ListUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return nil, fmt.Errorf("list inline policies for %s: %w", userName, err)
	}
	if listed == nil {
		return nil, nil
	}

	var dropped []string
	for _, name := range listed.PolicyNames {
		policyName := aws.StringValue(name)
		if policyName == "" || policyName == adminPrincipalPolicyName {
			continue
		}
		if _, err := svc.DeleteUserPolicy(accountID, &iam.DeleteUserPolicyInput{
			UserName:   aws.String(userName),
			PolicyName: name,
		}); err != nil {
			return nil, fmt.Errorf("remove inline policy %s from %s: %w", policyName, userName, err)
		}
		dropped = append(dropped, policyName)
	}
	return dropped, nil
}

// runAdminPrincipalDelete removes a principal entirely: its keys, its policies
// and the user. Revoking leaves a principal that can be re-issued; this is for
// one that should not exist at all.
func runAdminPrincipalDelete(_ *cobra.Command, args []string) error {
	userName := args[0]
	if err := validatePrincipalName(userName); err != nil {
		return err
	}

	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()
	if _, err := svc.GetUser(accountID, &iam.GetUserInput{UserName: aws.String(userName)}); err != nil {
		return fmt.Errorf("no principal %q in account %s", userName, accountID)
	}

	revoked, err := revokePrincipalKeys(svc, accountID, userName)
	if err != nil {
		return err
	}
	for _, keyID := range revoked {
		fmt.Printf("Revoked access key %s\n", keyID)
	}

	policies, err := svc.ListUserPolicies(accountID, &iam.ListUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return fmt.Errorf("list inline policies for %s: %w", userName, err)
	}
	if policies != nil {
		for _, name := range policies.PolicyNames {
			if _, err := svc.DeleteUserPolicy(accountID, &iam.DeleteUserPolicyInput{
				UserName:   aws.String(userName),
				PolicyName: name,
			}); err != nil {
				return fmt.Errorf("remove inline policy %s from %s: %w",
					aws.StringValue(name), userName, err)
			}
			fmt.Printf("Removed inline policy %s\n", aws.StringValue(name))
		}
	}

	if _, err := svc.DeleteUser(accountID, &iam.DeleteUserInput{
		UserName: aws.String(userName),
	}); err != nil {
		return fmt.Errorf("delete user %s: %w", userName, err)
	}

	fmt.Printf("\nPrincipal %q removed from account %s.\n", userName, accountID)
	return nil
}

// revokePrincipalKeys deletes every access key the user holds and returns the
// IDs. A key from an earlier run is unrecoverable to its holder but still
// authenticates, so it is removed rather than left live alongside a new one.
func revokePrincipalKeys(svc handlers_iam.IAMService, accountID, userName string) ([]string, error) {
	listed, err := svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return nil, fmt.Errorf("list access keys for %s: %w", userName, err)
	}
	if listed == nil {
		return nil, nil
	}

	var revoked []string
	for _, meta := range listed.AccessKeyMetadata {
		if meta == nil || meta.AccessKeyId == nil {
			continue
		}
		if _, err := svc.DeleteAccessKey(accountID, &iam.DeleteAccessKeyInput{
			UserName:    aws.String(userName),
			AccessKeyId: meta.AccessKeyId,
		}); err != nil {
			return nil, fmt.Errorf("remove access key for %s: %w", userName, err)
		}
		revoked = append(revoked, aws.StringValue(meta.AccessKeyId))
	}
	return revoked, nil
}

// resolvePrincipalGrants turns the requested method names into the actions the
// policy allows. An empty request means every admin method: a principal minted
// without a --grant is the operator credential.
func resolvePrincipalGrants(requested []string) ([]string, error) {
	known := gateway.AdminMethodNames()
	if len(requested) == 0 {
		return prefixGrants(known), nil
	}

	byLower := make(map[string]string, len(known))
	for _, method := range known {
		byLower[strings.ToLower(method)] = method
	}

	seen := make(map[string]bool, len(requested))
	methods := make([]string, 0, len(requested))
	for _, entry := range requested {
		name := strings.TrimSpace(entry)
		name = strings.TrimPrefix(name, adminPrincipalService+":")
		method, ok := byLower[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("unknown admin method %q — the surface serves: %s",
				entry, strings.Join(known, ", "))
		}
		if seen[method] {
			continue
		}
		seen[method] = true
		methods = append(methods, method)
	}

	sort.Strings(methods)
	return prefixGrants(methods), nil
}

// prefixGrants qualifies method names with the service scope the gateway
// evaluates them under.
func prefixGrants(methods []string) []string {
	grants := make([]string, 0, len(methods))
	for _, method := range methods {
		grants = append(grants, adminPrincipalService+":"+method)
	}
	return grants
}

// principalPolicyDocument builds the inline policy. Every method is named:
// granting spinifex:* would authorise whatever is added to the admin surface
// next with a key minted before it existed.
func principalPolicyDocument(grants []string) string {
	quoted := make([]string, 0, len(grants))
	for _, grant := range grants {
		quoted = append(quoted, `"`+grant+`"`)
	}
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` +
		strings.Join(quoted, ",") + `],"Resource":"*"}]}`
}

// validatePrincipalName rejects names that would collide with the account's own
// administrator or that IAM would not accept.
func validatePrincipalName(name string) error {
	if name == "" {
		return errors.New("principal name is required")
	}
	if name == handlers_iam.AdminUserName {
		return fmt.Errorf("%q is the super-admin account's own administrator — "+
			"rotating its key would lock out the bootstrap credential; choose another name",
			handlers_iam.AdminUserName)
	}
	if len(name) > 64 {
		return errors.New("principal name must be 64 characters or fewer")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '+', r == '=', r == ',', r == '@':
		default:
			return fmt.Errorf("principal name %q contains an unsupported character %q", name, r)
		}
	}
	return nil
}

// runAdminPrincipalAudit reports roles in the super-admin account that any
// principal in the account can assume, which includes every admin principal.
func runAdminPrincipalAudit(_ *cobra.Command, _ []string) error {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()
	roles, err := svc.ListRoles(accountID, &iam.ListRolesInput{})
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}

	var flagged []string
	for _, role := range roles.Roles {
		if role == nil || role.AssumeRolePolicyDocument == nil {
			continue
		}
		if trustsWholeAccount(aws.StringValue(role.AssumeRolePolicyDocument), accountID) {
			flagged = append(flagged, aws.StringValue(role.RoleName))
		}
	}

	if len(flagged) == 0 {
		fmt.Printf("No roles in %s trust the account as a whole.\n", accountID)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Roles in %s assumable by any principal in the account, including every admin principal:\n", accountID)
	for _, name := range flagged {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
	return fmt.Errorf("%d role(s) trust the account as a whole — scope their trust policies to a specific principal", len(flagged))
}

// trustsWholeAccount reports whether an AssumeRolePolicyDocument admits every
// principal in accountID. A malformed document is reported as trusting, since
// a document nobody can parse is not a document anybody has verified.
func trustsWholeAccount(document, accountID string) bool {
	doc, err := handlers_iam.ValidateTrustPolicyDocument(document)
	if err != nil {
		return true
	}
	for _, stmt := range doc.Statement {
		if stmt.Effect != "Allow" {
			continue
		}
		var principal struct {
			AWS handlers_iam.StringOrArr `json:"AWS"`
		}
		if err := json.Unmarshal(stmt.Principal, &principal); err != nil {
			continue
		}
		for _, entry := range principal.AWS {
			switch entry {
			case "*", accountID, "arn:aws:iam::" + accountID + ":root":
				return true
			}
		}
	}
	return false
}

var adminPrincipalCmd = &cobra.Command{
	Use:   "principal",
	Short: "Manage the credentials that call the private admin API",
	Long: `Manage the IAM users in the super-admin account that sign POST /admin/<Method>
requests.

The private admin API creates, lists and deletes tenant accounts. It is not an
AWS API and is unreachable until a principal exists — there is no config toggle
and no default credential for it.`,
}

var adminPrincipalCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create an admin principal and print its access key once",
	Long: `Create an IAM user in the super-admin account with an inline policy allowing the
requested admin methods, then mint an access key.

Without --grant the principal may call every admin method, which is the
credential an operator or a test harness uses. Pass --grant to scope it: a
self-service signup form, for example, needs only CreateAccount, and a key with
that grant cannot delete anything if the form's host is compromised.

Each method is granted by name rather than with a wildcard, so a later addition
to the admin surface is not authorised by an existing key.

The principal's grants are exactly one inline policy: re-running with fewer
--grant values narrows it, and any other inline policy on the user is removed.

The secret is printed once and never recoverable. Re-running replaces the access
key, which revokes the previous one.

  spx admin principal create operator
  spx admin principal create signup --grant CreateAccount
  spx admin principal create auditor --grant ListAccounts,DescribeAccountDeletion`,
	Args:         cobra.ExactArgs(1),
	RunE:         runAdminPrincipalCreate,
	SilenceUsage: true,
}

var adminPrincipalListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List the principals in the super-admin account and what they may call",
	Args:         cobra.NoArgs,
	RunE:         runAdminPrincipalList,
	SilenceUsage: true,
}

var adminPrincipalRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Revoke a principal's access keys without removing the principal",
	Long: `Delete every access key the named principal holds. The user and its policy stay,
so re-issuing is one "principal create" away.

Revocation is immediate and cluster-wide.`,
	Args:         cobra.ExactArgs(1),
	RunE:         runAdminPrincipalRevoke,
	SilenceUsage: true,
}

var adminPrincipalDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Remove a principal, its policies and its access keys",
	Long: `Delete the named principal from the super-admin account entirely.

Use "principal revoke" instead to stop a credential working while keeping the
principal, which is what a rotation after a leak needs.`,
	Args:         cobra.ExactArgs(1),
	RunE:         runAdminPrincipalDelete,
	SilenceUsage: true,
}

var adminPrincipalAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Flag roles an admin principal could assume",
	Long: `Report roles in the super-admin account whose trust policy names the account,
its root ARN, or a wildcard.

STS does not evaluate the caller's identity policy on AssumeRole, so any such
role is assumable by a scoped admin principal, which then inherits its
permissions. Roles trusting a service principal are unaffected.

Exits non-zero if any role is flagged.`,
	Args:         cobra.NoArgs,
	RunE:         runAdminPrincipalAudit,
	SilenceUsage: true,
}

func init() {
	adminCmd.AddCommand(adminPrincipalCmd)
	adminPrincipalCmd.AddCommand(adminPrincipalCreateCmd)
	adminPrincipalCmd.AddCommand(adminPrincipalListCmd)
	adminPrincipalCmd.AddCommand(adminPrincipalRevokeCmd)
	adminPrincipalCmd.AddCommand(adminPrincipalDeleteCmd)
	adminPrincipalCmd.AddCommand(adminPrincipalAuditCmd)

	adminPrincipalCreateCmd.Flags().StringSlice("grant", nil,
		"Admin methods this principal may call (default: all of "+
			strings.Join(gateway.AdminMethodNames(), ", ")+")")
}
