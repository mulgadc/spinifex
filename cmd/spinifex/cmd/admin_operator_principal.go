package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/spf13/cobra"
)

// OperatorPrincipalUserName is the IAM user in the super-admin account that an
// operator or a test harness signs /admin/ requests with. It is separate from
// the signup Worker's credential so revoking either costs nothing else.
const OperatorPrincipalUserName = "operator"

// operatorPrincipalPolicyName is the inline policy attached to that user.
const operatorPrincipalPolicyName = "operator-admin-methods"

// operatorPrincipalPolicyDocument grants the admin methods one by one rather
// than as spinifex:*, so a leaked operator key does not become a standing
// wildcard over whatever is added to the surface next.
const operatorPrincipalPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` +
	`"spinifex:CreateAccount",` +
	`"spinifex:DeleteAccount",` +
	`"spinifex:DescribeAccountDeletion",` +
	`"spinifex:ListAccounts"` +
	`],"Resource":"*"}]}`

// operatorPrincipalActions is what the summary line reports. A test asserts it
// matches the document above, which stays a literal so a security review reads
// the policy rather than the code that builds it.
var operatorPrincipalActions = []string{
	"spinifex:CreateAccount",
	"spinifex:DeleteAccount",
	"spinifex:DescribeAccountDeletion",
	"spinifex:ListAccounts",
}

var operatorPrincipalCmd = &cobra.Command{
	Use:   "operator-principal",
	Short: "Manage the credential an operator calls the private admin API with",
	Long: `Manage the IAM user in the super-admin account that operators and the load-test
harness sign POST /admin/<Method> requests with.`,
}

var operatorPrincipalCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create the operator principal and print its access key once",
	Long: `Create the "operator" IAM user in the super-admin account with an inline policy
allowing CreateAccount, DeleteAccount, DescribeAccountDeletion and ListAccounts,
then mint an access key.

Each method is granted by name rather than with a wildcard, so a later addition
to the admin surface is not authorised by an existing key.

The secret is printed once and never recoverable. Store it as an AWS profile;
never commit it and never place it in a Spinifex config file.

Re-running replaces the access key, which revokes the previous one.`,
	Run: runOperatorPrincipalCreate,
}

func init() {
	adminCmd.AddCommand(operatorPrincipalCmd)
	operatorPrincipalCmd.AddCommand(operatorPrincipalCreateCmd)
}

// runOperatorPrincipalCreate provisions the operator principal in the
// super-admin account. Every step is create-if-absent except the access key,
// which is replaced so exactly one is ever live.
func runOperatorPrincipalCreate(_ *cobra.Command, _ []string) {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()

	if _, err := svc.CreateUser(accountID, &iam.CreateUserInput{
		UserName: aws.String(OperatorPrincipalUserName),
	}); err != nil && !strings.Contains(err.Error(), "EntityAlreadyExists") {
		fmt.Fprintf(os.Stderr, "Error creating operator user: %v\n", err)
		os.Exit(1)
	}

	if _, err := svc.PutUserPolicy(accountID, &iam.PutUserPolicyInput{
		UserName:       aws.String(OperatorPrincipalUserName),
		PolicyName:     aws.String(operatorPrincipalPolicyName),
		PolicyDocument: aws.String(operatorPrincipalPolicyDocument),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Error attaching operator policy: %v\n", err)
		os.Exit(1)
	}

	// A key from a previous run is unrecoverable to its holder but still
	// authenticates, so it is removed rather than left live alongside the new one.
	listed, err := svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{
		UserName: aws.String(OperatorPrincipalUserName),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing operator access keys: %v\n", err)
		os.Exit(1)
	}
	if listed != nil {
		for _, meta := range listed.AccessKeyMetadata {
			if meta == nil || meta.AccessKeyId == nil {
				continue
			}
			if _, err := svc.DeleteAccessKey(accountID, &iam.DeleteAccessKeyInput{
				UserName:    aws.String(OperatorPrincipalUserName),
				AccessKeyId: meta.AccessKeyId,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing previous operator access key: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Revoked previous access key %s\n", aws.StringValue(meta.AccessKeyId))
		}
	}

	akOut, err := svc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{
		UserName: aws.String(OperatorPrincipalUserName),
	})
	if err != nil || akOut == nil || akOut.AccessKey == nil {
		fmt.Fprintf(os.Stderr, "Error creating operator access key: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nOperator principal ready.")
	fmt.Printf("  Account ID:        %s\n", accountID)
	fmt.Printf("  User:              %s\n", OperatorPrincipalUserName)
	fmt.Printf("  Permitted actions: %s\n", strings.Join(operatorPrincipalActions, ", "))
	fmt.Printf("  Access Key ID:     %s\n", aws.StringValue(akOut.AccessKey.AccessKeyId))
	fmt.Printf("  Secret Access Key: %s\n", aws.StringValue(akOut.AccessKey.SecretAccessKey))
	fmt.Println("\nThe secret is shown once. Store it as an AWS profile and call the surface with:")
	fmt.Println("  spx admin account list --remote --endpoint https://<node>:9999 --region <region>")
}
