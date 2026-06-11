package handlers_iam

import (
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Interface compliance check
var _ IAMService = (*IAMServiceImpl)(nil)

const testAccountID = utils.GlobalAccountID

func setupTestIAMService(t *testing.T) *IAMServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := GenerateMasterKey()
	require.NoError(t, err)

	svc, err := NewIAMServiceImpl(nc, masterKey, 1)
	require.NoError(t, err)
	return svc
}

func createTestUser(t *testing.T, svc *IAMServiceImpl, userName string) *iam.User {
	t.Helper()
	out, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String(userName),
	})
	require.NoError(t, err)
	return out.User
}

// ============================================================================
// User CRUD Tests
// ============================================================================

func TestCreateUser(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("testuser"),
		Path:     aws.String("/developers/"),
		Tags: []*iam.Tag{
			{Key: aws.String("team"), Value: aws.String("backend")},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, out.User)
	assert.Equal(t, "testuser", *out.User.UserName)
	assert.Contains(t, *out.User.Arn, "testuser")
	assert.Equal(t, "/developers/", *out.User.Path)
	assert.True(t, len(*out.User.UserId) > 4)
	assert.Equal(t, "AIDA", (*out.User.UserId)[:4])
}

func TestCreateUser_DefaultPath(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("defaultpath"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.User.Path)
}

func TestCreateUser_Duplicate(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("duplicateuser"),
	})
	require.NoError(t, err)

	_, err = svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("duplicateuser"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestGetUser(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "getuser")

	out, err := svc.GetUser(testAccountID, &iam.GetUserInput{
		UserName: aws.String("getuser"),
	})
	require.NoError(t, err)
	assert.Equal(t, "getuser", *out.User.UserName)
}

func TestGetUser_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetUser(testAccountID, &iam.GetUserInput{
		UserName: aws.String("nonexistent"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListUsers(t *testing.T) {
	svc := setupTestIAMService(t)

	createTestUser(t, svc, "user1")
	createTestUser(t, svc, "user2")
	createTestUser(t, svc, "user3")

	out, err := svc.ListUsers(testAccountID, &iam.ListUsersInput{})
	require.NoError(t, err)
	assert.Len(t, out.Users, 3)

	names := make(map[string]bool)
	for _, u := range out.Users {
		names[*u.UserName] = true
	}
	assert.True(t, names["user1"])
	assert.True(t, names["user2"])
	assert.True(t, names["user3"])
}

func TestListUsers_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.ListUsers(testAccountID, &iam.ListUsersInput{})
	require.NoError(t, err)
	assert.Len(t, out.Users, 0)
}

func TestListUsers_PathFilter(t *testing.T) {
	svc := setupTestIAMService(t)

	svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("dev1"),
		Path:     aws.String("/developers/"),
	})
	svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("admin1"),
		Path:     aws.String("/admins/"),
	})

	out, err := svc.ListUsers(testAccountID, &iam.ListUsersInput{
		PathPrefix: aws.String("/developers/"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Users, 1)
	assert.Equal(t, "dev1", *out.Users[0].UserName)
}

func TestDeleteUser(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "deleteuser")

	_, err := svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("deleteuser"),
	})
	require.NoError(t, err)

	_, err = svc.GetUser(testAccountID, &iam.GetUserInput{
		UserName: aws.String("deleteuser"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteUser_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("nonexistent"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteUser_WithAccessKeys(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "userWithKeys")

	_, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("userWithKeys"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("userWithKeys"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

func TestDeleteUser_WithAttachedPolicies(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "policyuser")

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("TestPolicy"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
	})
	require.NoError(t, err)

	policyARN := "arn:aws:iam::" + testAccountID + ":policy/TestPolicy"
	_, err = svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("policyuser"),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("policyuser"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

// ============================================================================
// Input Validation Tests
// ============================================================================

func TestCreateUser_InvalidName_TooLong(t *testing.T) {
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 65)

	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String(longName),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateUser_InvalidName_BadChars(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("user name!"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateUser_InvalidPath(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("validuser"),
		Path:     aws.String("no-leading-slash/"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreatePolicy_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 129)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(longName),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreatePolicy_InvalidPath(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("ValidPolicy"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
		Path:           aws.String("bad-path"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestValidatePolicyDocument_TooLarge(t *testing.T) {
	largeDoc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"` + strings.Repeat("a", maxPolicyDocumentSize) + `"}]}`
	_, err := ValidatePolicyDocument(largeDoc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

// ============================================================================
// Access Key Tests
// ============================================================================

func TestCreateAccessKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "keyuser")

	out, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("keyuser"),
	})
	require.NoError(t, err)
	require.NotNil(t, out.AccessKey)

	assert.Equal(t, "keyuser", *out.AccessKey.UserName)
	assert.Equal(t, "Active", *out.AccessKey.Status)
	assert.True(t, len(*out.AccessKey.AccessKeyId) >= 20)
	assert.Equal(t, "AKIA", (*out.AccessKey.AccessKeyId)[:4])
	assert.True(t, len(*out.AccessKey.SecretAccessKey) >= 30)
}

func TestCreateAccessKey_SecretIsDecryptable(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "decryptuser")

	out, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("decryptuser"),
	})
	require.NoError(t, err)

	plaintextSecret := *out.AccessKey.SecretAccessKey
	accessKeyID := *out.AccessKey.AccessKeyId

	// Look up the stored key and verify the encrypted secret can be decrypted
	ak, err := svc.LookupAccessKey(accessKeyID)
	require.NoError(t, err)

	decrypted, err := DecryptSecret(ak.SecretAccessKey, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, plaintextSecret, decrypted)
}

func TestCreateAccessKey_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("nonexistent"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestCreateAccessKey_MaxLimit(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "limituser")

	_, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("limituser"),
	})
	require.NoError(t, err)

	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("limituser"),
	})
	require.NoError(t, err)

	// Third key should fail (AWS limit is 2)
	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("limituser"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)
}

func TestAccessKeyQuota_Recovery(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "quotauser")

	// Create 2 keys (at limit)
	key1, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("quotauser"),
	})
	require.NoError(t, err)

	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("quotauser"),
	})
	require.NoError(t, err)

	// Delete 1
	_, err = svc.DeleteAccessKey(testAccountID, &iam.DeleteAccessKeyInput{
		UserName:    aws.String("quotauser"),
		AccessKeyId: key1.AccessKey.AccessKeyId,
	})
	require.NoError(t, err)

	// Create another — should succeed (back under quota)
	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("quotauser"),
	})
	assert.NoError(t, err, "should be able to create key after deleting one")
}

func TestAccessKeyQuota_PerUser(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "user1")
	createTestUser(t, svc, "user2")

	// Fill user1's quota
	_, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String("user1")})
	require.NoError(t, err)
	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String("user1")})
	require.NoError(t, err)

	// user2 should still be able to create keys
	_, err = svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String("user2")})
	assert.NoError(t, err, "user2 quota should be independent of user1")
}

func TestListAccessKeys(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "listkeysuser")

	key1, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("listkeysuser"),
	})
	key2, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("listkeysuser"),
	})

	out, err := svc.ListAccessKeys(testAccountID, &iam.ListAccessKeysInput{
		UserName: aws.String("listkeysuser"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AccessKeyMetadata, 2)

	keyIDs := make(map[string]bool)
	for _, k := range out.AccessKeyMetadata {
		keyIDs[*k.AccessKeyId] = true
	}
	assert.True(t, keyIDs[*key1.AccessKey.AccessKeyId])
	assert.True(t, keyIDs[*key2.AccessKey.AccessKeyId])
}

func TestDeleteAccessKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "delkeyuser")

	keyOut, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("delkeyuser"),
	})
	require.NoError(t, err)
	keyID := *keyOut.AccessKey.AccessKeyId

	_, err = svc.DeleteAccessKey(testAccountID, &iam.DeleteAccessKeyInput{
		UserName:    aws.String("delkeyuser"),
		AccessKeyId: aws.String(keyID),
	})
	require.NoError(t, err)

	listOut, err := svc.ListAccessKeys(testAccountID, &iam.ListAccessKeysInput{
		UserName: aws.String("delkeyuser"),
	})
	require.NoError(t, err)
	assert.Len(t, listOut.AccessKeyMetadata, 0)

	// User should now be deletable (no access keys)
	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("delkeyuser"),
	})
	require.NoError(t, err)
}

func TestDeleteAccessKey_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "delnotfounduser")

	_, err := svc.DeleteAccessKey(testAccountID, &iam.DeleteAccessKeyInput{
		UserName:    aws.String("delnotfounduser"),
		AccessKeyId: aws.String("AKIANONEXISTENT12345"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestUpdateAccessKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "updatekeyuser")

	keyOut, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("updatekeyuser"),
	})
	keyID := *keyOut.AccessKey.AccessKeyId

	// Deactivate
	_, err := svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: aws.String(keyID),
		Status:      aws.String("Inactive"),
	})
	require.NoError(t, err)

	// Verify status changed
	listOut, _ := svc.ListAccessKeys(testAccountID, &iam.ListAccessKeysInput{
		UserName: aws.String("updatekeyuser"),
	})
	assert.Equal(t, "Inactive", *listOut.AccessKeyMetadata[0].Status)

	// Reactivate
	_, err = svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: aws.String(keyID),
		Status:      aws.String("Active"),
	})
	require.NoError(t, err)

	listOut, _ = svc.ListAccessKeys(testAccountID, &iam.ListAccessKeysInput{
		UserName: aws.String("updatekeyuser"),
	})
	assert.Equal(t, "Active", *listOut.AccessKeyMetadata[0].Status)
}

func TestUpdateAccessKey_InvalidStatus(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "invalidstatususer")

	keyOut, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("invalidstatususer"),
	})

	_, err := svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
		Status:      aws.String("Invalid"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestUpdateAccessKey_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: aws.String("AKIANONEXISTENT12345"),
		Status:      aws.String("Inactive"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestUpdateAccessKey_CaseSensitiveStatus(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "caseuser")

	keyOut, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("caseuser"),
	})

	// "active" (lowercase) should be rejected — must be "Active"
	_, err := svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
		Status:      aws.String("active"),
	})
	assert.Error(t, err, "lowercase 'active' should be rejected")

	// "inactive" (lowercase) should be rejected
	_, err = svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
		Status:      aws.String("inactive"),
	})
	assert.Error(t, err, "lowercase 'inactive' should be rejected")

	// "ACTIVE" (uppercase) should be rejected
	_, err = svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
		Status:      aws.String("ACTIVE"),
	})
	assert.Error(t, err, "uppercase 'ACTIVE' should be rejected")
}

func TestUpdateAccessKey_CrossAccountBlocked(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Status Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Status Org B")
	require.NoError(t, err)

	_, err = svc.CreateUser(accA.AccountID, &iam.CreateUserInput{UserName: aws.String("alice")})
	require.NoError(t, err)

	keyOut, err := svc.CreateAccessKey(accA.AccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("alice"),
	})
	require.NoError(t, err)

	// Account B trying to update Account A's key should fail
	_, err = svc.UpdateAccessKey(accB.AccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
		Status:      aws.String("Inactive"),
	})
	assert.Error(t, err, "cross-account key update should be blocked")
}

// ============================================================================
// Auth Tests (LookupAccessKey)
// ============================================================================

func TestLookupAccessKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "lookupuser")

	keyOut, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("lookupuser"),
	})
	require.NoError(t, err)

	ak, err := svc.LookupAccessKey(*keyOut.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, "lookupuser", ak.UserName)
	assert.Equal(t, "Active", ak.Status)
	assert.NotEmpty(t, ak.SecretAccessKey) // encrypted secret
}

func TestLookupAccessKey_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.LookupAccessKey("AKIANONEXISTENT12345")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestLookupAccessKey_InactiveKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "inactiveuser")

	keyOut, _ := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("inactiveuser"),
	})
	keyID := *keyOut.AccessKey.AccessKeyId

	svc.UpdateAccessKey(testAccountID, &iam.UpdateAccessKeyInput{
		AccessKeyId: aws.String(keyID),
		Status:      aws.String("Inactive"),
	})

	// LookupAccessKey should still return the key — status check is the caller's job
	ak, err := svc.LookupAccessKey(keyID)
	require.NoError(t, err)
	assert.Equal(t, "Inactive", ak.Status)
}

// ============================================================================
// SeedBootstrap Tests
// ============================================================================

func TestSeedBootstrap(t *testing.T) {
	svc := setupTestIAMService(t)

	encryptedSecret, err := EncryptSecret("test-secret-key", svc.masterKey)
	require.NoError(t, err)

	err = svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIAEXAMPLE123456789",
		EncryptedSecret: encryptedSecret,
		AccountID:       utils.GlobalAccountID,
	})
	require.NoError(t, err)

	// Verify root user exists at account-scoped key
	out, err := svc.GetUser(utils.GlobalAccountID, &iam.GetUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err)
	assert.Equal(t, "root", *out.User.UserName)
	assert.Contains(t, *out.User.Arn, utils.GlobalAccountID)
	assert.Contains(t, *out.User.Arn, "root")

	// Verify access key exists with AccountID
	ak, err := svc.LookupAccessKey("AKIAEXAMPLE123456789")
	require.NoError(t, err)
	assert.Equal(t, "root", ak.UserName)
	assert.Equal(t, utils.GlobalAccountID, ak.AccountID)
	assert.Equal(t, "Active", ak.Status)

	// Verify secret is decryptable
	decrypted, err := DecryptSecret(ak.SecretAccessKey, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, "test-secret-key", decrypted)

	// Verify global account record was created
	account, err := svc.GetAccount(utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Equal(t, utils.GlobalAccountID, account.AccountID)
	assert.Equal(t, "system", account.AccountName)
	assert.Equal(t, "ACTIVE", account.Status)
}

func TestSeedBootstrap_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)

	encryptedSecret, _ := EncryptSecret("test-secret", svc.masterKey)
	data := &BootstrapData{
		AccessKeyID:     "AKIAEXAMPLE123456789",
		EncryptedSecret: encryptedSecret,
		AccountID:       utils.GlobalAccountID,
	}

	// First call seeds
	err := svc.SeedBootstrap(data)
	require.NoError(t, err)

	// Second call should succeed (no-op, idempotent)
	err = svc.SeedBootstrap(data)
	require.NoError(t, err)

	// Root user should still exist with original data
	out, err := svc.GetUser(utils.GlobalAccountID, &iam.GetUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err)
	assert.Equal(t, "root", *out.User.UserName)
}

func TestSeedBootstrap_WithAdmin(t *testing.T) {
	svc := setupTestIAMService(t)

	systemSecret, err := EncryptSecret("system-secret", svc.masterKey)
	require.NoError(t, err)
	adminSecret, err := EncryptSecret("admin-secret", svc.masterKey)
	require.NoError(t, err)

	err = svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIASYSTEM1234567890",
		EncryptedSecret: systemSecret,
		AccountID:       utils.GlobalAccountID,
		Admin: &AdminBootstrapData{
			AccountID:       "000000000001",
			AccountName:     "spinifex",
			UserName:        "admin",
			AccessKeyID:     "AKIAADMIN12345678901",
			EncryptedSecret: adminSecret,
		},
	})
	require.NoError(t, err)

	// Verify system root user exists
	out, err := svc.GetUser(utils.GlobalAccountID, &iam.GetUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err)
	assert.Equal(t, "root", *out.User.UserName)

	// Verify admin account created
	account, err := svc.GetAccount("000000000001")
	require.NoError(t, err)
	assert.Equal(t, "000000000001", account.AccountID)
	assert.Equal(t, "spinifex", account.AccountName)
	assert.Equal(t, "ACTIVE", account.Status)

	// Verify admin user exists
	adminOut, err := svc.GetUser("000000000001", &iam.GetUserInput{
		UserName: aws.String("admin"),
	})
	require.NoError(t, err)
	assert.Equal(t, "admin", *adminOut.User.UserName)
	assert.Contains(t, *adminOut.User.Arn, "000000000001")

	// Verify admin access key
	ak, err := svc.LookupAccessKey("AKIAADMIN12345678901")
	require.NoError(t, err)
	assert.Equal(t, "admin", ak.UserName)
	assert.Equal(t, "000000000001", ak.AccountID)

	decrypted, err := DecryptSecret(ak.SecretAccessKey, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, "admin-secret", decrypted)

	// Verify AdministratorAccess policy was created and attached
	policies, err := svc.GetUserPolicies("000000000001", "admin")
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, "Allow", policies[0].Statement[0].Effect)
	assert.Equal(t, StringOrArr{"*"}, policies[0].Statement[0].Action)
	assert.Equal(t, StringOrArr{"*"}, policies[0].Statement[0].Resource)

	// Verify account counter set to 2 (next CreateAccount gets 000000000002)
	nextAccount, err := svc.CreateAccount("test-org")
	require.NoError(t, err)
	assert.Equal(t, "000000000002", nextAccount.AccountID)
}

func TestSeedBootstrap_AdminNil_BackwardCompat(t *testing.T) {
	svc := setupTestIAMService(t)

	encryptedSecret, err := EncryptSecret("test-secret", svc.masterKey)
	require.NoError(t, err)

	// No Admin field — backward compatibility with old bootstrap.json
	err = svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIAEXAMPLE123456789",
		EncryptedSecret: encryptedSecret,
		AccountID:       utils.GlobalAccountID,
	})
	require.NoError(t, err)

	// Root user should exist
	out, err := svc.GetUser(utils.GlobalAccountID, &iam.GetUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err)
	assert.Equal(t, "root", *out.User.UserName)

	// Admin account should NOT exist
	_, err = svc.GetAccount("000000000001")
	assert.Error(t, err)
}

// ============================================================================
// IsEmpty Tests
// ============================================================================

func TestIsEmpty_True(t *testing.T) {
	svc := setupTestIAMService(t)

	empty, err := svc.IsEmpty()
	require.NoError(t, err)
	assert.True(t, empty)
}

func TestIsEmpty_False(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "notempty")

	empty, err := svc.IsEmpty()
	require.NoError(t, err)
	assert.False(t, empty)
}

// ============================================================================
// Helper Function Tests
// ============================================================================

func TestGenerateIAMID(t *testing.T) {
	id, err := generateIAMID("AIDA")
	assert.NoError(t, err)
	assert.Equal(t, "AIDA", id[:4])
	assert.True(t, len(id) == 21) // AIDA + 17 hex chars

	// Two IDs should differ
	id2, err := generateIAMID("AIDA")
	assert.NoError(t, err)
	assert.NotEqual(t, id, id2)

	// Policy ID prefix
	pid, err := generateIAMID("ANPA")
	assert.NoError(t, err)
	assert.Equal(t, "ANPA", pid[:4])
	assert.Len(t, pid, 21)
}

func TestGenerateAccessKeyID(t *testing.T) {
	id, err := generateAccessKeyID()
	assert.NoError(t, err)
	assert.Equal(t, "AKIA", id[:4])
	assert.True(t, len(id) == 24) // AKIA + 20 hex chars

	id2, err := generateAccessKeyID()
	assert.NoError(t, err)
	assert.NotEqual(t, id, id2)
}

func TestGenerateSecretAccessKey(t *testing.T) {
	secret, err := admin.GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.Len(t, secret, 40)

	secret2, err := admin.GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.NotEqual(t, secret, secret2)
}

// ============================================================================
// Policy CRUD Tests
// ============================================================================

// validPolicyDocument returns a valid IAM policy document JSON string.
func validPolicyDocument() string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeInstances","Resource":"*"}]}`
}

func createTestPolicy(t *testing.T, svc *IAMServiceImpl, name string) *iam.Policy {
	t.Helper()
	out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(name),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)
	return out.Policy
}

func TestCreatePolicy(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("AllowEC2"),
		PolicyDocument: aws.String(validPolicyDocument()),
		Path:           aws.String("/devteam/"),
		Description:    aws.String("Allow EC2 describe"),
	})
	require.NoError(t, err)
	require.NotNil(t, out.Policy)
	assert.Equal(t, "AllowEC2", *out.Policy.PolicyName)
	assert.Equal(t, "/devteam/", *out.Policy.Path)
	assert.Equal(t, "Allow EC2 describe", *out.Policy.Description)
	assert.Equal(t, "v1", *out.Policy.DefaultVersionId)
	assert.Contains(t, *out.Policy.Arn, "policy/devteam/AllowEC2")
	assert.True(t, len(*out.Policy.PolicyId) > 4)
	assert.Equal(t, "ANPA", (*out.Policy.PolicyId)[:4])
	assert.Equal(t, int64(0), *out.Policy.AttachmentCount)
	assert.True(t, *out.Policy.IsAttachable)
}

func TestCreatePolicy_DefaultPath(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("DefaultPath"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.Policy.Path)
	assert.Contains(t, *out.Policy.Arn, "policy/DefaultPath")
}

func TestCreatePolicy_InvalidJSON(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("BadJSON"),
		PolicyDocument: aws.String(`{not valid json`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_InvalidVersion(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("BadVersion"),
		PolicyDocument: aws.String(`{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_NoStatements(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("NoStmts"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_InvalidEffect(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("BadEffect"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"*","Resource":"*"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_MissingAction(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("NoAction"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"*"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_MissingResource(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("NoResource"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicy_Duplicate(t *testing.T) {
	svc := setupTestIAMService(t)

	createTestPolicy(t, svc, "DupPolicy")

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("DupPolicy"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestCreatePolicy_ArrayActions(t *testing.T) {
	svc := setupTestIAMService(t)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:DescribeInstances","ec2:RunInstances"],"Resource":"*"}]}`
	out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("ArrayActions"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)
	assert.Equal(t, "ArrayActions", *out.Policy.PolicyName)
}

func TestGetPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "GetMe")

	out, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)
	assert.Equal(t, "GetMe", *out.Policy.PolicyName)
	assert.Equal(t, *created.PolicyId, *out.Policy.PolicyId)
	assert.Equal(t, *created.Arn, *out.Policy.Arn)
	assert.Equal(t, "v1", *out.Policy.DefaultVersionId)
	assert.Equal(t, int64(0), *out.Policy.AttachmentCount)
}

func TestGetPolicy_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Nonexistent"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetPolicy_MalformedARN(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
		PolicyArn: aws.String("not-an-arn"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetPolicy_ARNParsingEdgeCases(t *testing.T) {
	svc := setupTestIAMService(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

	// Create a real policy so we can test ARN mismatches
	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("TestARN"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)

	testCases := []struct {
		name string
		arn  string
	}{
		{"wrong partition", "arn:aws-cn:iam::" + testAccountID + ":policy/TestARN"},
		{"wrong service", "arn:aws:s3::" + testAccountID + ":policy/TestARN"},
		{"extra path segments", "arn:aws:iam::" + testAccountID + ":policy/extra/path/TestARN"},
		{"trailing slash only", "arn:aws:iam::" + testAccountID + ":policy/"},
		{"empty after policy", "arn:aws:iam::" + testAccountID + ":policy"},
		{"no policy segment", "arn:aws:iam::" + testAccountID + ":user/TestARN"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
				PolicyArn: aws.String(tc.arn),
			})
			assert.Error(t, err, "ARN %q should fail", tc.arn)
		})
	}
}

func TestGetPolicy_WithAttachments(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "AttachedPolicy")
	createTestUser(t, svc, "attachuser1")
	createTestUser(t, svc, "attachuser2")

	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("attachuser1"),
		PolicyArn: created.Arn,
	})
	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("attachuser2"),
		PolicyArn: created.Arn,
	})

	out, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), *out.Policy.AttachmentCount)
}

func TestGetPolicyVersion(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "VersionPolicy")

	out, err := svc.GetPolicyVersion(testAccountID, &iam.GetPolicyVersionInput{
		PolicyArn: created.Arn,
		VersionId: aws.String("v1"),
	})
	require.NoError(t, err)
	assert.Equal(t, "v1", *out.PolicyVersion.VersionId)
	assert.True(t, *out.PolicyVersion.IsDefaultVersion)
	assert.NotEmpty(t, *out.PolicyVersion.Document)

	// Verify the returned document is valid JSON
	doc, err := ValidatePolicyDocument(*out.PolicyVersion.Document)
	require.NoError(t, err)
	assert.Equal(t, "2012-10-17", doc.Version)
	assert.Len(t, doc.Statement, 1)
}

func TestGetPolicyVersion_InvalidVersion(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "VersionPolicy2")

	_, err := svc.GetPolicyVersion(testAccountID, &iam.GetPolicyVersionInput{
		PolicyArn: created.Arn,
		VersionId: aws.String("v2"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetPolicyVersion_PolicyNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetPolicyVersion(testAccountID, &iam.GetPolicyVersionInput{
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Ghost"),
		VersionId: aws.String("v1"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListPolicyVersions(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "ListVersionPolicy")

	out, err := svc.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)
	assert.False(t, *out.IsTruncated)
	require.Len(t, out.Versions, 1)
	assert.Equal(t, "v1", *out.Versions[0].VersionId)
	assert.True(t, *out.Versions[0].IsDefaultVersion)
	// AWS omits the document from list entries — callers fetch it via GetPolicyVersion.
	assert.Nil(t, out.Versions[0].Document)
}

func TestListPolicyVersions_PolicyNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListPolicies(t *testing.T) {
	svc := setupTestIAMService(t)

	createTestPolicy(t, svc, "Policy1")
	createTestPolicy(t, svc, "Policy2")
	createTestPolicy(t, svc, "Policy3")

	out, err := svc.ListPolicies(testAccountID, &iam.ListPoliciesInput{})
	require.NoError(t, err)
	assert.Len(t, out.Policies, 3)

	names := make(map[string]bool)
	for _, p := range out.Policies {
		names[*p.PolicyName] = true
	}
	assert.True(t, names["Policy1"])
	assert.True(t, names["Policy2"])
	assert.True(t, names["Policy3"])
}

func TestListPolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.ListPolicies(testAccountID, &iam.ListPoliciesInput{})
	require.NoError(t, err)
	assert.Len(t, out.Policies, 0)
}

func TestDeletePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "DeleteMe")

	_, err := svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	// Confirm it's gone
	_, err = svc.GetPolicy(testAccountID, &iam.GetPolicyInput{
		PolicyArn: created.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeletePolicy_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeletePolicy_AttachedConflict(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "Attached")
	createTestUser(t, svc, "conflictuser")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("conflictuser"),
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{
		PolicyArn: created.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)

	// Detach, then delete should succeed
	_, err = svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{
		UserName:  aws.String("conflictuser"),
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)
}

// ============================================================================
// Policy Attachment Tests
// ============================================================================

func TestAttachUserPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "AttachPolicy")
	createTestUser(t, svc, "attachme")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("attachme"),
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	// Verify via list
	out, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("attachme"),
	})
	require.NoError(t, err)
	require.Len(t, out.AttachedPolicies, 1)
	assert.Equal(t, "AttachPolicy", *out.AttachedPolicies[0].PolicyName)
	assert.Equal(t, *created.Arn, *out.AttachedPolicies[0].PolicyArn)
}

func TestAttachUserPolicy_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "IdempotentPolicy")
	createTestUser(t, svc, "idempotentuser")

	input := &iam.AttachUserPolicyInput{
		UserName:  aws.String("idempotentuser"),
		PolicyArn: created.Arn,
	}

	_, err := svc.AttachUserPolicy(testAccountID, input)
	require.NoError(t, err)

	// Attach same policy again — should succeed silently
	_, err = svc.AttachUserPolicy(testAccountID, input)
	require.NoError(t, err)

	// Should still have exactly 1 attachment
	out, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("idempotentuser"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 1)
}

func TestAttachUserPolicy_NonexistentPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "orphanuser")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("orphanuser"),
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAttachUserPolicy_NonexistentUser(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "OrphanPolicy")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("ghostuser"),
		PolicyArn: created.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDetachUserPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "DetachPolicy")
	createTestUser(t, svc, "detachme")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("detachme"),
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{
		UserName:  aws.String("detachme"),
		PolicyArn: created.Arn,
	})
	require.NoError(t, err)

	// Verify detached
	out, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("detachme"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

func TestDetachUserPolicy_NotAttached(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestPolicy(t, svc, "NeverAttached")
	createTestUser(t, svc, "cleanuser")

	_, err := svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{
		UserName:  aws.String("cleanuser"),
		PolicyArn: created.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDetachUserPolicy_NonexistentUser(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{
		UserName:  aws.String("ghostuser"),
		PolicyArn: aws.String("arn:aws:iam::000000000000:policy/Whatever"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListAttachedUserPolicies(t *testing.T) {
	svc := setupTestIAMService(t)

	p1 := createTestPolicy(t, svc, "ListPolicy1")
	p2 := createTestPolicy(t, svc, "ListPolicy2")
	createTestUser(t, svc, "listpuser")

	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("listpuser"),
		PolicyArn: p1.Arn,
	})
	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("listpuser"),
		PolicyArn: p2.Arn,
	})

	out, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("listpuser"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 2)

	names := make(map[string]bool)
	for _, p := range out.AttachedPolicies {
		names[*p.PolicyName] = true
	}
	assert.True(t, names["ListPolicy1"])
	assert.True(t, names["ListPolicy2"])
}

func TestListAttachedUserPolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "nopolicies")

	out, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("nopolicies"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

func TestListAttachedUserPolicies_NonexistentUser(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("ghostuser"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// ============================================================================
// GetUserPolicies (internal) Tests
// ============================================================================

func TestGetUserPolicies(t *testing.T) {
	svc := setupTestIAMService(t)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeInstances","Resource":"*"}]}`
	p1, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("InternalPolicy1"),
		PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)

	doc2 := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"ec2:TerminateInstances","Resource":"*"}]}`
	p2, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("InternalPolicy2"),
		PolicyDocument: aws.String(doc2),
	})
	require.NoError(t, err)

	createTestUser(t, svc, "evaluser")

	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("evaluser"),
		PolicyArn: p1.Policy.Arn,
	})
	svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("evaluser"),
		PolicyArn: p2.Policy.Arn,
	})

	docs, err := svc.GetUserPolicies(testAccountID, "evaluser")
	require.NoError(t, err)
	assert.Len(t, docs, 2)
	assert.Equal(t, "2012-10-17", docs[0].Version)
	assert.Equal(t, "2012-10-17", docs[1].Version)
}

func TestGetUserPolicies_NoPolicies(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "emptyuser")

	docs, err := svc.GetUserPolicies(testAccountID, "emptyuser")
	require.NoError(t, err)
	assert.Len(t, docs, 0)
}

func TestGetUserPolicies_NonexistentUser(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetUserPolicies(testAccountID, "ghostuser")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// ============================================================================
// ValidatePolicyDocument Tests
// ============================================================================

func TestValidatePolicyDocument_Valid(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	require.NoError(t, err)
	assert.Equal(t, "2012-10-17", doc.Version)
	assert.Len(t, doc.Statement, 1)
	assert.Equal(t, "Allow", doc.Statement[0].Effect)
}

func TestValidatePolicyDocument_MultipleStatements(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[
			{"Sid":"AllowEC2","Effect":"Allow","Action":["ec2:DescribeInstances","ec2:RunInstances"],"Resource":"*"},
			{"Effect":"Deny","Action":"ec2:TerminateInstances","Resource":"*"}
		]
	}`)
	require.NoError(t, err)
	assert.Len(t, doc.Statement, 2)
	assert.Equal(t, "AllowEC2", doc.Statement[0].Sid)
	assert.Len(t, doc.Statement[0].Action, 2)
}

func TestValidatePolicyDocument_BadJSON(t *testing.T) {
	_, err := ValidatePolicyDocument(`{broken`)
	assert.Error(t, err)
}

func TestValidatePolicyDocument_WrongVersion(t *testing.T) {
	_, err := ValidatePolicyDocument(`{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported policy version")
}

func TestValidatePolicyDocument_EmptyStatements(t *testing.T) {
	_, err := ValidatePolicyDocument(`{"Version":"2012-10-17","Statement":[]}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one statement")
}

func TestValidatePolicyDocument_BadEffect(t *testing.T) {
	_, err := ValidatePolicyDocument(`{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"*","Resource":"*"}]}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Effect must be Allow or Deny")
}

func TestValidatePolicyDocument_MissingAction(t *testing.T) {
	_, err := ValidatePolicyDocument(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"*"}]}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Action is required")
}

func TestValidatePolicyDocument_MissingResource(t *testing.T) {
	_, err := ValidatePolicyDocument(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*"}]}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Resource is required")
}

// ============================================================================
// Sensitive Data Not Logged Tests
// ============================================================================

func TestSensitiveDataNotLogged_CreateAccessKey(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "loguser")

	// Capture log output
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	})

	akOut, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("loguser"),
	})
	require.NoError(t, err)

	secretKey := *akOut.AccessKey.SecretAccessKey
	logOutput := buf.String()

	// The plaintext secret key must never appear in logs
	assert.NotContains(t, logOutput, secretKey,
		"plaintext secret access key must not appear in log output")

	// The encrypted secret should also not appear in logs
	ak, err := svc.LookupAccessKey(*akOut.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.NotContains(t, logOutput, ak.SecretAccessKey,
		"encrypted secret must not appear in log output")
}

func TestSensitiveDataNotLogged_MasterKey(t *testing.T) {
	// Capture log output during service init
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	})

	svc := setupTestIAMService(t)

	logOutput := buf.String()
	masterKeyHex := hex.EncodeToString(svc.masterKey)

	assert.NotContains(t, logOutput, masterKeyHex,
		"master key hex must not appear in log output")
	assert.NotContains(t, logOutput, string(svc.masterKey),
		"raw master key bytes must not appear in log output")
}

// ============================================================================
// Input Validation Boundary Tests
// ============================================================================

func TestInputValidation_UserNameLength(t *testing.T) {
	svc := setupTestIAMService(t)

	// 64 chars — should pass
	name64 := strings.Repeat("a", 64)
	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{UserName: aws.String(name64)})
	assert.NoError(t, err, "64-char username should be valid")

	// 65 chars — should fail
	name65 := strings.Repeat("a", 65)
	_, err = svc.CreateUser(testAccountID, &iam.CreateUserInput{UserName: aws.String(name65)})
	assert.Error(t, err, "65-char username should be rejected")
}

func TestInputValidation_PolicyNameLength(t *testing.T) {
	svc := setupTestIAMService(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

	// 128 chars — should pass
	name128 := strings.Repeat("P", 128)
	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(name128),
		PolicyDocument: aws.String(doc),
	})
	assert.NoError(t, err, "128-char policy name should be valid")

	// 129 chars — should fail
	name129 := strings.Repeat("P", 129)
	_, err = svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(name129),
		PolicyDocument: aws.String(doc),
	})
	assert.Error(t, err, "129-char policy name should be rejected")
}

func TestInputValidation_PathLength(t *testing.T) {
	svc := setupTestIAMService(t)

	// 512 chars — should pass (path must start and end with /)
	path512 := "/" + strings.Repeat("a", 510) + "/"
	assert.Len(t, path512, 512)
	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("pathuser512"),
		Path:     aws.String(path512),
	})
	assert.NoError(t, err, "512-char path should be valid")

	// 513 chars — should fail
	path513 := "/" + strings.Repeat("a", 511) + "/"
	assert.Len(t, path513, 513)
	_, err = svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("pathuser513"),
		Path:     aws.String(path513),
	})
	assert.Error(t, err, "513-char path should be rejected")
}

func TestInputValidation_PolicyDocumentSize(t *testing.T) {
	// 6144 bytes — should pass
	filler6144 := strings.Repeat("a", 6144-len(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"`)-len(`"}]}`))
	doc6144 := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"` + filler6144 + `"}]}`
	assert.Len(t, doc6144, 6144)
	_, err := ValidatePolicyDocument(doc6144)
	assert.NoError(t, err, "6144-byte policy document should be valid")

	// 6145 bytes — should fail
	doc6145 := doc6144 + " "
	_, err = ValidatePolicyDocument(doc6145)
	assert.Error(t, err, "6145-byte policy document should be rejected")
}

func TestValidatePolicyDocument_OverlappingDenyAllow(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[
			{"Effect":"Allow","Action":"ec2:*","Resource":"*"},
			{"Effect":"Deny","Action":"ec2:TerminateInstances","Resource":"*"},
			{"Effect":"Allow","Action":"ec2:TerminateInstances","Resource":"arn:aws:ec2:*:*:instance/i-abc123"}
		]
	}`)
	require.NoError(t, err)
	assert.Len(t, doc.Statement, 3)
	assert.Equal(t, PolicyEffectAllow, doc.Statement[0].Effect)
	assert.Equal(t, PolicyEffectDeny, doc.Statement[1].Effect)
	assert.Equal(t, PolicyEffectAllow, doc.Statement[2].Effect)
}

func TestValidatePolicyDocument_WithoutSid(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]
	}`)
	require.NoError(t, err)
	assert.Empty(t, doc.Statement[0].Sid, "Sid should be empty when omitted")
}

func TestValidatePolicyDocument_ActionSingleString(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]
	}`)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{"s3:*"}, doc.Statement[0].Action)
}

func TestValidatePolicyDocument_ActionArray(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":["s3:Get*","s3:List*"],"Resource":"*"}]
	}`)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{"s3:Get*", "s3:List*"}, doc.Statement[0].Action)
}

func TestValidatePolicyDocument_EmptyActionArray(t *testing.T) {
	_, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":[],"Resource":"*"}]
	}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Action is required")
}

func TestValidatePolicyDocument_EmptyResourceArray(t *testing.T) {
	_, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":[]}]
	}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Resource is required")
}

func TestValidatePolicyDocument_ResourceARNFormat(t *testing.T) {
	doc, err := ValidatePolicyDocument(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::my-bucket/*"}]
	}`)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{"arn:aws:s3:::my-bucket/*"}, doc.Statement[0].Resource)
}

// ============================================================================
// Helper Function Tests (Policy)
// ============================================================================

func TestGeneratePolicyID(t *testing.T) {
	id, err := generateIAMID("ANPA")
	assert.NoError(t, err)
	assert.Equal(t, "ANPA", id[:4])
	assert.Len(t, id, 21) // ANPA + 17 hex chars

	id2, err := generateIAMID("ANPA")
	assert.NoError(t, err)
	assert.NotEqual(t, id, id2)
}

// ============================================================================
// Validator Tests
// ============================================================================

func TestIsIAMNameChar(t *testing.T) {
	// Valid characters
	for _, c := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+=,.@-_" {
		assert.True(t, isIAMNameChar(byte(c)), "expected valid: %c", c)
	}
	// Invalid characters
	for _, c := range " !#$%^&*(){}[]|\\:;\"'<>?/`~\t\n" {
		assert.False(t, isIAMNameChar(byte(c)), "expected invalid: %c", c)
	}
}

func TestValidateUserName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "alice", false},
		{"valid with special chars", "alice.bob+test@example_com", false},
		{"valid single char", "a", false},
		{"valid 64 chars", strings.Repeat("a", 64), false},
		{"empty", "", true},
		{"too long 65 chars", strings.Repeat("a", 65), true},
		{"invalid space", "alice bob", true},
		{"invalid slash", "alice/bob", true},
		{"invalid colon", "alice:bob", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUserName(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidatePolicyName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "MyPolicy", false},
		{"valid with special chars", "My.Policy-v2_test+1", false},
		{"valid single char", "P", false},
		{"valid 128 chars", strings.Repeat("x", 128), false},
		{"empty", "", true},
		{"too long 129 chars", strings.Repeat("x", 129), true},
		{"invalid space", "My Policy", true},
		{"invalid slash", "My/Policy", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePolicyName(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"root path", "/", false},
		{"nested path", "/division/engineering/", false},
		{"no leading slash", "division/", true},
		{"no trailing slash", "/division", true},
		{"empty string", "", true},
		{"just text", "division", true},
		{"max length 512", "/" + strings.Repeat("a", 510) + "/", false},
		{"over max length 513", "/" + strings.Repeat("a", 511) + "/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePath(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseCreatedAt(t *testing.T) {
	// Valid RFC3339
	ts := "2024-01-15T10:30:00Z"
	result := parseCreatedAt(ts)
	assert.Equal(t, 2024, result.Year())
	assert.Equal(t, time.January, result.Month())
	assert.Equal(t, 15, result.Day())
	assert.Equal(t, 10, result.Hour())
	assert.Equal(t, 30, result.Minute())

	// With timezone offset
	ts2 := "2024-06-01T12:00:00+05:00"
	result2 := parseCreatedAt(ts2)
	assert.Equal(t, 2024, result2.Year())
	assert.Equal(t, time.June, result2.Month())

	// Invalid — returns zero time (fallback)
	result3 := parseCreatedAt("not-a-date")
	assert.True(t, result3.IsZero())

	// Empty string — returns zero time
	result4 := parseCreatedAt("")
	assert.True(t, result4.IsZero())
}

func TestGenerateIAMID_AllUpperHex(t *testing.T) {
	for range 20 {
		id, err := generateIAMID("AIDA")
		assert.NoError(t, err)
		suffix := id[4:]
		for _, c := range suffix {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F'),
				"expected uppercase hex char, got %c in ID %s", c, id)
		}
	}
}

func TestGenerateAccessKeyID_AllUpperHex(t *testing.T) {
	for range 20 {
		id, err := generateAccessKeyID()
		assert.NoError(t, err)
		suffix := id[4:]
		for _, c := range suffix {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F'),
				"expected uppercase hex char, got %c in ID %s", c, id)
		}
	}
}
