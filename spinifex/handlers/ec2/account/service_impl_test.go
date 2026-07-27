package handlers_ec2_account

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAccountSettingsServiceImplWithNATS_NoJetStream verifies that creating
// the service against a NATS server without JetStream returns an error instead
// of silently degrading.
func TestNewAccountSettingsServiceImplWithNATS_NoJetStream(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	svc, err := NewAccountSettingsServiceImplWithNATS(t.Context(), nil, nc)
	assert.Error(t, err)
	assert.Nil(t, svc)
}

var _ AccountSettingsService = (*AccountSettingsServiceImpl)(nil)

func setupTestAccountService(t *testing.T) *AccountSettingsServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	svc, err := NewAccountSettingsServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	return svc
}

const testAccountID = "111111111111"

// EBS Encryption tests

func TestEbsEncryption_DefaultOff(t *testing.T) {
	svc := setupTestAccountService(t)
	out, err := svc.GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *out.EbsEncryptionByDefault)
}

func TestEbsEncryption_EnableDisable(t *testing.T) {
	svc := setupTestAccountService(t)

	// Enable
	enableOut, err := svc.EnableEbsEncryptionByDefault(context.Background(), &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *enableOut.EbsEncryptionByDefault)

	// Verify
	getOut, err := svc.GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *getOut.EbsEncryptionByDefault)

	// Disable
	disableOut, err := svc.DisableEbsEncryptionByDefault(context.Background(), &ec2.DisableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *disableOut.EbsEncryptionByDefault)

	// Verify
	getOut, err = svc.GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *getOut.EbsEncryptionByDefault)
}

// Serial Console tests

func TestSerialConsole_DefaultOff(t *testing.T) {
	svc := setupTestAccountService(t)
	out, err := svc.GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *out.SerialConsoleAccessEnabled)
}

func TestSerialConsole_EnableDisable(t *testing.T) {
	svc := setupTestAccountService(t)

	enableOut, err := svc.EnableSerialConsoleAccess(context.Background(), &ec2.EnableSerialConsoleAccessInput{}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *enableOut.SerialConsoleAccessEnabled)

	getOut, err := svc.GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *getOut.SerialConsoleAccessEnabled)

	disableOut, err := svc.DisableSerialConsoleAccess(context.Background(), &ec2.DisableSerialConsoleAccessInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *disableOut.SerialConsoleAccessEnabled)

	getOut, err = svc.GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *getOut.SerialConsoleAccessEnabled)
}

// Multi-account isolation tests

func TestEbsEncryption_MultiAccountIsolation(t *testing.T) {
	svc := setupTestAccountService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Enable EBS encryption for account A only
	_, err := svc.EnableEbsEncryptionByDefault(context.Background(), &ec2.EnableEbsEncryptionByDefaultInput{}, accountA)
	require.NoError(t, err)

	// Account A should have encryption enabled
	outA, err := svc.GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, accountA)
	require.NoError(t, err)
	assert.True(t, *outA.EbsEncryptionByDefault)

	// Account B should still have encryption disabled (default)
	outB, err := svc.GetEbsEncryptionByDefault(context.Background(), &ec2.GetEbsEncryptionByDefaultInput{}, accountB)
	require.NoError(t, err)
	assert.False(t, *outB.EbsEncryptionByDefault)
}

func TestSerialConsole_MultiAccountIsolation(t *testing.T) {
	svc := setupTestAccountService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Enable serial console for account A only
	_, err := svc.EnableSerialConsoleAccess(context.Background(), &ec2.EnableSerialConsoleAccessInput{}, accountA)
	require.NoError(t, err)

	// Account A should have serial console enabled
	outA, err := svc.GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, accountA)
	require.NoError(t, err)
	assert.True(t, *outA.SerialConsoleAccessEnabled)

	// Account B should still have serial console disabled (default)
	outB, err := svc.GetSerialConsoleAccessStatus(context.Background(), &ec2.GetSerialConsoleAccessStatusInput{}, accountB)
	require.NoError(t, err)
	assert.False(t, *outB.SerialConsoleAccessEnabled)
}

func TestSettingsKey_EmptyAccountIDFallback(t *testing.T) {
	assert.Equal(t, utils.GlobalAccountID, settingsKey(""))
	assert.Equal(t, "123456789012", settingsKey("123456789012"))
}
