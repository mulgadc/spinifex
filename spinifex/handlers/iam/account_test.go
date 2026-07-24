package handlers_iam

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Account CRUD Tests
// ============================================================================

func TestCreateAccount(t *testing.T) {
	svc := setupTestIAMService(t)

	acc1, err := svc.CreateAccount("Team Alpha")
	require.NoError(t, err)
	assert.Equal(t, "000000000001", acc1.AccountID)
	assert.Equal(t, "Team Alpha", acc1.AccountName)
	assert.Equal(t, "ACTIVE", acc1.Status)
	assert.NotEmpty(t, acc1.CreatedAt)

	// Second account gets sequential ID
	acc2, err := svc.CreateAccount("Team Beta")
	require.NoError(t, err)
	assert.Equal(t, "000000000002", acc2.AccountID)
	assert.Equal(t, "Team Beta", acc2.AccountName)
}

func TestCreateAccount_SequentialIDs(t *testing.T) {
	svc := setupTestIAMService(t)

	var prevID string
	for range 5 {
		acc, err := svc.CreateAccount("account")
		require.NoError(t, err)
		assert.Len(t, acc.AccountID, 12)
		if prevID != "" {
			assert.Greater(t, acc.AccountID, prevID)
		}
		prevID = acc.AccountID
	}
}

func TestGetAccount(t *testing.T) {
	svc := setupTestIAMService(t)

	created, err := svc.CreateAccount("Lookup Test")
	require.NoError(t, err)

	got, err := svc.GetAccount(created.AccountID)
	require.NoError(t, err)
	assert.Equal(t, created.AccountID, got.AccountID)
	assert.Equal(t, "Lookup Test", got.AccountName)
	assert.Equal(t, "ACTIVE", got.Status)
}

func TestGetAccount_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetAccount("999999999999")
	assert.Error(t, err)
}

func TestListAccounts(t *testing.T) {
	svc := setupTestIAMService(t)

	svc.CreateAccount("Acct1")
	svc.CreateAccount("Acct2")
	svc.CreateAccount("Acct3")

	accounts, err := svc.ListAccounts()
	require.NoError(t, err)
	assert.Len(t, accounts, 3)

	names := make(map[string]bool)
	for _, a := range accounts {
		names[a.AccountName] = true
	}
	assert.True(t, names["Acct1"])
	assert.True(t, names["Acct2"])
	assert.True(t, names["Acct3"])
}

func TestListAccounts_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	accounts, err := svc.ListAccounts()
	require.NoError(t, err)
	assert.Empty(t, accounts)
}

// ============================================================================
// Account-Scoped User Tests
// ============================================================================

func TestAccountScopedUsers(t *testing.T) {
	svc := setupTestIAMService(t)

	acc1, err := svc.CreateAccount("Org A")
	require.NoError(t, err)
	acc2, err := svc.CreateAccount("Org B")
	require.NoError(t, err)

	// Create same username in both accounts
	_, err = svc.CreateUser(acc1.AccountID, &iam.CreateUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)

	_, err = svc.CreateUser(acc2.AccountID, &iam.CreateUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)

	// Both should be independently retrievable
	out1, err := svc.GetUser(acc1.AccountID, &iam.GetUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)
	assert.Contains(t, *out1.User.Arn, acc1.AccountID)

	out2, err := svc.GetUser(acc2.AccountID, &iam.GetUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)
	assert.Contains(t, *out2.User.Arn, acc2.AccountID)

	// Listing users in acc1 should only return acc1's user
	list1, err := svc.ListUsers(acc1.AccountID, &iam.ListUsersInput{})
	require.NoError(t, err)
	assert.Len(t, list1.Users, 1)

	list2, err := svc.ListUsers(acc2.AccountID, &iam.ListUsersInput{})
	require.NoError(t, err)
	assert.Len(t, list2.Users, 1)

	// Deleting in one account shouldn't affect the other
	_, err = svc.DeleteUser(acc1.AccountID, &iam.DeleteUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)

	_, err = svc.GetUser(acc2.AccountID, &iam.GetUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err) // acc2's admin still exists
}

// ============================================================================
// Account-Scoped Policy Tests
// ============================================================================

func TestAccountScopedPolicies(t *testing.T) {
	svc := setupTestIAMService(t)

	acc1, err := svc.CreateAccount("Policy Org A")
	require.NoError(t, err)
	acc2, err := svc.CreateAccount("Policy Org B")
	require.NoError(t, err)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

	// Create same policy name in both accounts
	p1, err := svc.CreatePolicy(acc1.AccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("AdminAccess"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)
	assert.Contains(t, *p1.Policy.Arn, acc1.AccountID)

	p2, err := svc.CreatePolicy(acc2.AccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("AdminAccess"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)
	assert.Contains(t, *p2.Policy.Arn, acc2.AccountID)

	// Listing in each account should be independent
	list1, err := svc.ListPolicies(acc1.AccountID, &iam.ListPoliciesInput{})
	require.NoError(t, err)
	assert.Len(t, list1.Policies, 1)

	list2, err := svc.ListPolicies(acc2.AccountID, &iam.ListPoliciesInput{})
	require.NoError(t, err)
	assert.Len(t, list2.Policies, 1)
}

// ============================================================================
// Access Key Account ID Tests
// ============================================================================

func TestAccessKeyReturnsAccountID(t *testing.T) {
	svc := setupTestIAMService(t)

	acc, err := svc.CreateAccount("Key Org")
	require.NoError(t, err)

	_, err = svc.CreateUser(acc.AccountID, &iam.CreateUserInput{
		UserName: aws.String("keyuser"),
	})
	require.NoError(t, err)

	akOut, err := svc.CreateAccessKey(acc.AccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("keyuser"),
	})
	require.NoError(t, err)

	// LookupAccessKey should return the correct account ID
	ak, err := svc.LookupAccessKey(*akOut.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, acc.AccountID, ak.AccountID)
	assert.Equal(t, "keyuser", ak.UserName)
}

// ============================================================================
// SeedBootstrap Account-Scoped Tests
// ============================================================================

// ============================================================================
// Cross-Account Isolation Tests
// ============================================================================

func TestCrossAccount_UserIsolation(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Account A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Account B")
	require.NoError(t, err)

	// Create user in Account A
	_, err = svc.CreateUser(accA.AccountID, &iam.CreateUserInput{
		UserName: aws.String("alice"),
	})
	require.NoError(t, err)

	// Account B cannot get Account A's user
	_, err = svc.GetUser(accB.AccountID, &iam.GetUserInput{
		UserName: aws.String("alice"),
	})
	assert.Error(t, err, "should not be able to get Account A user from Account B")

	// Account B listing should not include Account A's user
	list, err := svc.ListUsers(accB.AccountID, &iam.ListUsersInput{})
	require.NoError(t, err)
	assert.Empty(t, list.Users, "Account B should have no users")
}

func TestCrossAccount_AccessKeyIsolation(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Key Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Key Org B")
	require.NoError(t, err)

	_, err = svc.CreateUser(accA.AccountID, &iam.CreateUserInput{
		UserName: aws.String("keyuser"),
	})
	require.NoError(t, err)

	akOut, err := svc.CreateAccessKey(accA.AccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("keyuser"),
	})
	require.NoError(t, err)

	// LookupAccessKey returns Account A's ID, not Account B's
	ak, err := svc.LookupAccessKey(*akOut.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, accA.AccountID, ak.AccountID, "access key must belong to Account A")
	assert.NotEqual(t, accB.AccountID, ak.AccountID, "access key must not belong to Account B")
}

func TestCrossAccount_PolicyAttachmentFails(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Policy Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Policy Org B")
	require.NoError(t, err)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`

	// Create policy in Account A
	pOut, err := svc.CreatePolicy(accA.AccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("S3Access"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)

	// Create user in Account B
	_, err = svc.CreateUser(accB.AccountID, &iam.CreateUserInput{
		UserName: aws.String("bob"),
	})
	require.NoError(t, err)

	// Attaching Account A's policy to Account B's user should fail
	_, err = svc.AttachUserPolicy(accB.AccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("bob"),
		PolicyArn: pOut.Policy.Arn,
	})
	assert.Error(t, err, "cross-account policy attachment should fail")
}

func TestCrossAccount_LookupAccessKeyReturnsCorrectAccount(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Lookup A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Lookup B")
	require.NoError(t, err)

	// Create same username in both accounts with access keys
	_, err = svc.CreateUser(accA.AccountID, &iam.CreateUserInput{UserName: aws.String("shared")})
	require.NoError(t, err)
	_, err = svc.CreateUser(accB.AccountID, &iam.CreateUserInput{UserName: aws.String("shared")})
	require.NoError(t, err)

	akA, err := svc.CreateAccessKey(accA.AccountID, &iam.CreateAccessKeyInput{UserName: aws.String("shared")})
	require.NoError(t, err)
	akB, err := svc.CreateAccessKey(accB.AccountID, &iam.CreateAccessKeyInput{UserName: aws.String("shared")})
	require.NoError(t, err)

	// Each key resolves to its own account
	lookupA, err := svc.LookupAccessKey(*akA.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, accA.AccountID, lookupA.AccountID)

	lookupB, err := svc.LookupAccessKey(*akB.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, accB.AccountID, lookupB.AccountID)

	// Keys are distinct
	assert.NotEqual(t, *akA.AccessKey.AccessKeyId, *akB.AccessKey.AccessKeyId)
}

// ============================================================================
// SeedBootstrap Account-Scoped Tests
// ============================================================================

func TestSeedBootstrap_AccountScoped(t *testing.T) {
	svc := setupTestIAMService(t)

	encryptedSecret, err := svc.key.EncryptBase64("root-secret")
	require.NoError(t, err)

	err = svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIAROOTEXAMPLE12345",
		EncryptedSecret: encryptedSecret,
		AccountID:       utils.GlobalAccountID,
	})
	require.NoError(t, err)

	// Root user stored at 000000000000:root
	out, err := svc.GetUser(utils.GlobalAccountID, &iam.GetUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err)
	assert.Equal(t, "root", *out.User.UserName)
	assert.Contains(t, *out.User.Arn, utils.GlobalAccountID)

	// Access key has correct AccountID
	ak, err := svc.LookupAccessKey("AKIAROOTEXAMPLE12345")
	require.NoError(t, err)
	assert.Equal(t, utils.GlobalAccountID, ak.AccountID)
	assert.Equal(t, "root", ak.UserName)

	// Global account record was created
	account, err := svc.GetAccount(utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Equal(t, utils.GlobalAccountID, account.AccountID)
	assert.Equal(t, "system", account.AccountName)
	assert.Equal(t, "ACTIVE", account.Status)
}

// ============================================================================
// Account Creation Event Tests
// ============================================================================

func TestCreateAccount_PublishesEvent(t *testing.T) {
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	masterKey, err := GenerateMasterKey()
	require.NoError(t, err)

	svc, err := NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("iam.account.created", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	acc, err := svc.CreateAccount("Event Test")
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		var evt struct {
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
		}
		require.NoError(t, json.Unmarshal(msg.Data, &evt))
		assert.Equal(t, acc.AccountID, evt.AccountID)
		assert.Equal(t, "Event Test", evt.AccountName)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for iam.account.created event")
	}
}
