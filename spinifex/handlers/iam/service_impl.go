package handlers_iam

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	KVBucketUsers            = "spinifex-iam-users"
	KVBucketAccessKeys       = "spinifex-iam-access-keys"
	KVBucketPolicies         = "spinifex-iam-policies"
	KVBucketAccounts         = "spinifex-accounts"
	KVBucketAccountCounter   = "spinifex-account-counter"
	KVBucketRoles            = "spinifex-iam-roles"
	KVBucketInstanceProfiles = "spinifex-iam-instance-profiles"

	KVBucketUsersVersion            = 1
	KVBucketAccessKeysVersion       = 1
	KVBucketPoliciesVersion         = 1
	KVBucketAccountsVersion         = 1
	KVBucketAccountCounterVersion   = 1
	KVBucketRolesVersion            = 1
	KVBucketInstanceProfilesVersion = 1

	maxAccessKeysPerUser = 2

	// LongLivedAccessKeyIDPrefix is the AWS-defined prefix for long-lived
	// IAM access keys. The access-keys bucket rejects writes whose key does
	// not start with it — a record under any other prefix in this bucket
	// would be resolved by the SigV4 AKIA path without the X-Amz-Security-Token
	// check that the ASIA path enforces, a silent privilege upgrade.
	LongLivedAccessKeyIDPrefix = "AKIA"
)

// putAccessKey writes an access-key record after enforcing the AKIA-prefix
// invariant. All writers to accessKeysBucket MUST go through this helper.
func (s *IAMServiceImpl) putAccessKey(accessKeyID string, data []byte) error {
	if !strings.HasPrefix(accessKeyID, LongLivedAccessKeyIDPrefix) {
		return fmt.Errorf("access key ID must start with %q, got %q",
			LongLivedAccessKeyIDPrefix, accessKeyID)
	}
	if _, err := s.accessKeysBucket.Put(accessKeyID, data); err != nil {
		return err
	}
	return nil
}

// createAccessKey writes an access-key record with CAS semantics (fails if the
// key already exists). Enforces the AKIA-prefix invariant.
func (s *IAMServiceImpl) createAccessKey(accessKeyID string, data []byte) error {
	if !strings.HasPrefix(accessKeyID, LongLivedAccessKeyIDPrefix) {
		return fmt.Errorf("access key ID must start with %q, got %q",
			LongLivedAccessKeyIDPrefix, accessKeyID)
	}
	if _, err := s.accessKeysBucket.Create(accessKeyID, data); err != nil {
		return err
	}
	return nil
}

// IAMServiceImpl implements IAM operations using NATS JetStream KV.
type IAMServiceImpl struct {
	js                     nats.JetStreamContext
	natsConn               *nats.Conn
	usersBucket            nats.KeyValue
	accessKeysBucket       nats.KeyValue
	policiesBucket         nats.KeyValue
	accountsBucket         nats.KeyValue
	accountCounterBucket   nats.KeyValue
	rolesBucket            nats.KeyValue
	instanceProfilesBucket nats.KeyValue
	masterKey              []byte
	decrypter              *Decrypter
}

var _ IAMService = (*IAMServiceImpl)(nil)

// NewIAMServiceImpl creates a new IAM service backed by NATS JetStream KV.
// clusterSize sets the JetStream replication factor for KV buckets. For
// multi-node clusters this must match the number of NATS servers so that
// buckets survive node failures. Pass 1 for single-node or test setups.
func NewIAMServiceImpl(natsConn *nats.Conn, masterKey []byte, clusterSize int) (*IAMServiceImpl, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}

	replicas := max(clusterSize, 1)

	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	usersBucket, err := getOrCreateBucket(js, KVBucketUsers, 10, replicas)
	if err != nil {
		return nil, fmt.Errorf("init users bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketUsers, usersBucket, KVBucketUsersVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketUsers, err)
	}

	accessKeysBucket, err := getOrCreateBucket(js, KVBucketAccessKeys, 5, replicas)
	if err != nil {
		return nil, fmt.Errorf("init access keys bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketAccessKeys, accessKeysBucket, KVBucketAccessKeysVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketAccessKeys, err)
	}

	policiesBucket, err := getOrCreateBucket(js, KVBucketPolicies, 10, replicas)
	if err != nil {
		return nil, fmt.Errorf("init policies bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketPolicies, policiesBucket, KVBucketPoliciesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketPolicies, err)
	}

	accountsBucket, err := getOrCreateBucket(js, KVBucketAccounts, 5, replicas)
	if err != nil {
		return nil, fmt.Errorf("init accounts bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketAccounts, accountsBucket, KVBucketAccountsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketAccounts, err)
	}

	accountCounterBucket, err := getOrCreateBucket(js, KVBucketAccountCounter, 5, replicas)
	if err != nil {
		return nil, fmt.Errorf("init account counter bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketAccountCounter, accountCounterBucket, KVBucketAccountCounterVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketAccountCounter, err)
	}

	rolesBucket, err := getOrCreateBucket(js, KVBucketRoles, 10, replicas)
	if err != nil {
		return nil, fmt.Errorf("init roles bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketRoles, rolesBucket, KVBucketRolesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketRoles, err)
	}

	instanceProfilesBucket, err := getOrCreateBucket(js, KVBucketInstanceProfiles, 10, replicas)
	if err != nil {
		return nil, fmt.Errorf("init instance profiles bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketInstanceProfiles, instanceProfilesBucket, KVBucketInstanceProfilesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketInstanceProfiles, err)
	}

	decrypter, err := NewDecrypter(masterKey)
	if err != nil {
		return nil, fmt.Errorf("init decrypter: %w", err)
	}

	slog.Info("IAM service initialized",
		"users_bucket", KVBucketUsers,
		"access_keys_bucket", KVBucketAccessKeys,
		"policies_bucket", KVBucketPolicies,
		"accounts_bucket", KVBucketAccounts,
		"roles_bucket", KVBucketRoles,
		"instance_profiles_bucket", KVBucketInstanceProfiles,
		"replicas", replicas)

	return &IAMServiceImpl{
		js:                     js,
		natsConn:               natsConn,
		usersBucket:            usersBucket,
		accessKeysBucket:       accessKeysBucket,
		policiesBucket:         policiesBucket,
		accountsBucket:         accountsBucket,
		accountCounterBucket:   accountCounterBucket,
		rolesBucket:            rolesBucket,
		instanceProfilesBucket: instanceProfilesBucket,
		masterKey:              masterKey,
		decrypter:              decrypter,
	}, nil
}

func getOrCreateBucket(js nats.JetStreamContext, name string, history uint8, replicas int) (nats.KeyValue, error) {
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   name,
		History:  history,
		Replicas: replicas,
	})
	if err != nil {
		kv, err = js.KeyValue(name)
		if err != nil {
			return nil, err
		}
	}
	return kv, nil
}

// ---------------------------------------------------------------------------
// User CRUD
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	userName := *input.UserName
	if err := validateUserName(userName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	path := "/"
	if input.Path != nil {
		path = *input.Path
		if err := validatePath(path); err != nil {
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
		}
	}

	userID, err := generateIAMID("AIDA")
	if err != nil {
		return nil, fmt.Errorf("generate user ID: %w", err)
	}

	kvKey := accountID + "." + userName
	user := User{
		UserName:         userName,
		UserID:           userID,
		AccountID:        accountID,
		ARN:              fmt.Sprintf("arn:aws:iam::%s:user%s%s", accountID, path, userName),
		Path:             path,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		AccessKeys:       []string{},
		Tags:             []Tag{},
		AttachedPolicies: []string{},
	}

	for _, tag := range input.Tags {
		if tag.Key != nil && tag.Value != nil {
			user.Tags = append(user.Tags, Tag{Key: *tag.Key, Value: *tag.Value})
		}
	}

	data, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	// Atomic create — fails if key already exists (race-safe)
	if _, err := s.usersBucket.Create(kvKey, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store user: %w", err)
	}

	slog.Info("IAM user created", "accountID", accountID, "userName", userName, "userID", user.UserID)

	createdAt := parseCreatedAt(user.CreatedAt)
	return &iam.CreateUserOutput{
		User: &iam.User{
			UserName:   aws.String(user.UserName),
			UserId:     aws.String(user.UserID),
			Arn:        aws.String(user.ARN),
			Path:       aws.String(user.Path),
			CreateDate: aws.Time(createdAt),
		},
	}, nil
}

func (s *IAMServiceImpl) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	user, err := s.getUser(accountID, *input.UserName)
	if err != nil {
		return nil, err
	}

	createdAt := parseCreatedAt(user.CreatedAt)
	return &iam.GetUserOutput{
		User: &iam.User{
			UserName:   aws.String(user.UserName),
			UserId:     aws.String(user.UserID),
			Arn:        aws.String(user.ARN),
			Path:       aws.String(user.Path),
			CreateDate: aws.Time(createdAt),
		},
	}, nil
}

func (s *IAMServiceImpl) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	keys, err := s.usersBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return &iam.ListUsersOutput{
				Users:       []*iam.User{},
				IsTruncated: aws.Bool(false),
			}, nil
		}
		return nil, fmt.Errorf("list user keys: %w", err)
	}

	pathPrefix := "/"
	if input.PathPrefix != nil {
		pathPrefix = *input.PathPrefix
	}

	keyPrefix := accountID + "."
	var users []*iam.User
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.usersBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("ListUsers: user key disappeared (concurrent delete)", "key", key)
			} else {
				slog.Warn("ListUsers: failed to get user", "key", key, "err", err)
			}
			continue
		}

		var user User
		if err := json.Unmarshal(entry.Value(), &user); err != nil {
			slog.Warn("ListUsers: failed to unmarshal user", "key", key, "err", err)
			continue
		}

		if !strings.HasPrefix(user.Path, pathPrefix) {
			continue
		}

		createdAt := parseCreatedAt(user.CreatedAt)
		users = append(users, &iam.User{
			UserName:   aws.String(user.UserName),
			UserId:     aws.String(user.UserID),
			Arn:        aws.String(user.ARN),
			Path:       aws.String(user.Path),
			CreateDate: aws.Time(createdAt),
		})
	}

	return &iam.ListUsersOutput{
		Users:       users,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	userName := *input.UserName
	kvKey := accountID + "." + userName

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	if len(user.AccessKeys) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}
	if len(user.AttachedPolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.usersBucket.Delete(kvKey); err != nil {
		return nil, fmt.Errorf("delete user: %w", err)
	}

	slog.Info("IAM user deleted", "accountID", accountID, "userName", userName)
	return &iam.DeleteUserOutput{}, nil
}

// ---------------------------------------------------------------------------
// Access Key Lifecycle
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	userName := *input.UserName
	userKVKey := accountID + "." + userName

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	if len(user.AccessKeys) >= maxAccessKeysPerUser {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}

	accessKeyID, err := generateAccessKeyID()
	if err != nil {
		return nil, fmt.Errorf("generate access key ID: %w", err)
	}
	secretAccessKey, err := admin.GenerateAWSSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	encryptedSecret, err := EncryptSecret(secretAccessKey, s.masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}

	ak := AccessKey{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: encryptedSecret,
		UserName:        userName,
		AccountID:       accountID,
		Status:          AccessKeyStatusActive,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}

	akData, err := json.Marshal(ak)
	if err != nil {
		return nil, fmt.Errorf("marshal access key: %w", err)
	}

	if err := s.putAccessKey(accessKeyID, akData); err != nil {
		return nil, fmt.Errorf("store access key: %w", err)
	}

	// Update user's access key list
	user.AccessKeys = append(user.AccessKeys, accessKeyID)
	userData, err := json.Marshal(user)
	if err != nil {
		if rbErr := s.accessKeysBucket.Delete(accessKeyID); rbErr != nil {
			slog.Error("Rollback failed: orphaned access key", "accessKeyID", accessKeyID, "err", rbErr)
		}
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		if rbErr := s.accessKeysBucket.Delete(accessKeyID); rbErr != nil {
			slog.Error("Rollback failed: orphaned access key", "accessKeyID", accessKeyID, "err", rbErr)
		}
		return nil, fmt.Errorf("update user: %w", err)
	}

	slog.Info("IAM access key created", "accountID", accountID, "userName", userName, "accessKeyID", accessKeyID)

	createdAt := parseCreatedAt(ak.CreatedAt)
	return &iam.CreateAccessKeyOutput{
		AccessKey: &iam.AccessKey{
			AccessKeyId:     aws.String(accessKeyID),
			SecretAccessKey: aws.String(secretAccessKey), // plaintext — only time it's returned
			UserName:        aws.String(userName),
			Status:          aws.String(AccessKeyStatusActive),
			CreateDate:      aws.Time(createdAt),
		},
	}, nil
}

func (s *IAMServiceImpl) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	user, err := s.getUser(accountID, *input.UserName)
	if err != nil {
		return nil, err
	}

	var metadata []*iam.AccessKeyMetadata
	for _, keyID := range user.AccessKeys {
		entry, err := s.accessKeysBucket.Get(keyID)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("ListAccessKeys: access key disappeared (concurrent delete)", "keyID", keyID)
			} else {
				slog.Warn("ListAccessKeys: failed to get access key", "keyID", keyID, "err", err)
			}
			continue
		}

		var ak AccessKey
		if err := json.Unmarshal(entry.Value(), &ak); err != nil {
			slog.Warn("ListAccessKeys: failed to unmarshal access key", "keyID", keyID, "err", err)
			continue
		}

		createdAt := parseCreatedAt(ak.CreatedAt)
		metadata = append(metadata, &iam.AccessKeyMetadata{
			AccessKeyId: aws.String(ak.AccessKeyID),
			UserName:    aws.String(ak.UserName),
			Status:      aws.String(ak.Status),
			CreateDate:  aws.Time(createdAt),
		})
	}

	return &iam.ListAccessKeysOutput{
		AccessKeyMetadata: metadata,
		IsTruncated:       aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	userName := *input.UserName
	accessKeyID := *input.AccessKeyId
	userKVKey := accountID + "." + userName

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	// Find and remove the access key reference from the user
	found := false
	remaining := make([]string, 0, len(user.AccessKeys))
	for _, keyID := range user.AccessKeys {
		if keyID == accessKeyID {
			found = true
		} else {
			remaining = append(remaining, keyID)
		}
	}

	if !found {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	// Update user record first, then delete the key. This avoids orphaning the
	// reference if a crash occurs between the two operations.
	user.AccessKeys = remaining
	userData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	if err := s.accessKeysBucket.Delete(accessKeyID); err != nil {
		return nil, fmt.Errorf("delete access key: %w", err)
	}

	slog.Info("IAM access key deleted", "accountID", accountID, "userName", userName, "accessKeyID", accessKeyID)
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (s *IAMServiceImpl) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	status := *input.Status
	if status != AccessKeyStatusActive && status != AccessKeyStatusInactive {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	accessKeyID := *input.AccessKeyId

	entry, err := s.accessKeysBucket.Get(accessKeyID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get access key: %w", err)
	}

	var ak AccessKey
	if err := json.Unmarshal(entry.Value(), &ak); err != nil {
		return nil, fmt.Errorf("unmarshal access key: %w", err)
	}

	if ak.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	ak.Status = status
	data, err := json.Marshal(ak)
	if err != nil {
		return nil, fmt.Errorf("marshal access key: %w", err)
	}

	if err := s.putAccessKey(accessKeyID, data); err != nil {
		return nil, fmt.Errorf("update access key: %w", err)
	}

	slog.Info("IAM access key updated", "accountID", accountID, "accessKeyID", accessKeyID, "status", status)
	return &iam.UpdateAccessKeyOutput{}, nil
}

// ---------------------------------------------------------------------------
// Auth (internal — used by SigV4 middleware and bootstrap)
// ---------------------------------------------------------------------------

// LookupAccessKey retrieves an access key by its ID. Returns the full record
// including the encrypted secret, for use by the SigV4 middleware.
func (s *IAMServiceImpl) LookupAccessKey(accessKeyID string) (*AccessKey, error) {
	entry, err := s.accessKeysBucket.Get(accessKeyID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("lookup access key: %w", err)
	}

	var ak AccessKey
	if err := json.Unmarshal(entry.Value(), &ak); err != nil {
		return nil, fmt.Errorf("unmarshal access key: %w", err)
	}
	return &ak, nil
}

// DecryptSecret decrypts a base64-encoded AES-256-GCM ciphertext using the
// pre-computed cipher initialized at startup.
func (s *IAMServiceImpl) DecryptSecret(ciphertext string) (string, error) {
	return s.decrypter.Decrypt(ciphertext)
}

// SeedBootstrap consumes bootstrap data to create the system root IAM user and
// (optionally) the default admin account in NATS KV. Uses conditional create
// (put-if-not-exists) for multi-node race safety — the first node wins; others
// skip silently. Also creates the system account record (000000000000).
// If data.Admin is non-nil, it also seeds the admin account, user, access key,
// AdministratorAccess policy, and sets the account counter so subsequent
// CreateAccount calls start at 000000000002.
func (s *IAMServiceImpl) SeedBootstrap(data *BootstrapData) error {
	// --- Seed system account (000000000000) ---
	systemAccount := Account{
		AccountID:   utils.GlobalAccountID,
		AccountName: "system",
		Status:      AccountStatusActive,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	accountData, err := json.Marshal(systemAccount)
	if err != nil {
		return fmt.Errorf("marshal system account: %w", err)
	}
	if _, err := s.accountsBucket.Create(utils.GlobalAccountID, accountData); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("seed system account: %w", err)
	}

	kvKey := utils.GlobalAccountID + ".root"
	rootUser := User{
		UserName:         "root",
		UserID:           "AIDAAAAAAAAAAAAAAAAA",
		AccountID:        utils.GlobalAccountID,
		ARN:              fmt.Sprintf("arn:aws:iam::%s:root", utils.GlobalAccountID),
		Path:             "/",
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		AccessKeys:       []string{data.AccessKeyID},
		Tags:             []Tag{},
		AttachedPolicies: []string{},
	}

	userData, err := json.Marshal(rootUser)
	if err != nil {
		return fmt.Errorf("marshal root user: %w", err)
	}

	// Conditional create — fails if key already exists (another node seeded first)
	_, err = s.usersBucket.Create(kvKey, userData)
	if errors.Is(err, nats.ErrKeyExists) {
		slog.Info("Root user already seeded by another node, skipping")
	} else if err != nil {
		return fmt.Errorf("seed root user: %w", err)
	}

	// Create access key entry (also conditional)
	ak := AccessKey{
		AccessKeyID:     data.AccessKeyID,
		SecretAccessKey: data.EncryptedSecret,
		UserName:        "root",
		AccountID:       utils.GlobalAccountID,
		Status:          AccessKeyStatusActive,
		CreatedAt:       rootUser.CreatedAt,
	}

	akData, err := json.Marshal(ak)
	if err != nil {
		return fmt.Errorf("marshal root access key: %w", err)
	}

	err = s.createAccessKey(data.AccessKeyID, akData)
	if errors.Is(err, nats.ErrKeyExists) {
		slog.Info("Root access key already seeded by another node, skipping")
	} else if err != nil {
		return fmt.Errorf("seed root access key: %w", err)
	} else {
		slog.Info("System root user seeded", "accountID", utils.GlobalAccountID, "accessKeyID", data.AccessKeyID)
	}

	// --- Seed admin account (000000000001) if present ---
	if data.Admin != nil {
		if err := s.seedAdminAccount(data.Admin); err != nil {
			return fmt.Errorf("seed admin account: %w", err)
		}
	}

	return nil
}

// seedAdminAccount creates the default admin account, user, access key,
// AdministratorAccess policy, and sets the account counter to 2.
func (s *IAMServiceImpl) seedAdminAccount(admin *AdminBootstrapData) error {
	now := time.Now().UTC().Format(time.RFC3339)

	// Create admin account record
	account := Account{
		AccountID:   admin.AccountID,
		AccountName: admin.AccountName,
		Status:      AccountStatusActive,
		CreatedAt:   now,
	}
	accountData, err := json.Marshal(account)
	if err != nil {
		return fmt.Errorf("marshal admin account: %w", err)
	}
	if _, err := s.accountsBucket.Create(admin.AccountID, accountData); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("store admin account: %w", err)
	}

	// Set account counter to 2 so next CreateAccount gets 000000000002
	if _, err := s.accountCounterBucket.Create("next_id", []byte("2")); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("seed account counter: %w", err)
	}

	// Create AdministratorAccess policy
	policyARN := fmt.Sprintf("arn:aws:iam::%s:policy/AdministratorAccess", admin.AccountID)
	policyDoc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	policyID, err := generateIAMID("ANPA")
	if err != nil {
		return fmt.Errorf("generate policy ID: %w", err)
	}
	policy := Policy{
		PolicyName:     "AdministratorAccess",
		PolicyID:       policyID,
		ARN:            policyARN,
		Path:           "/",
		Description:    "Full administrator access",
		PolicyDocument: policyDoc,
		CreatedAt:      now,
		DefaultVersion: "v1",
		Tags:           []Tag{},
	}
	policyData, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("marshal admin policy: %w", err)
	}
	policyKVKey := admin.AccountID + ".AdministratorAccess"
	if _, err := s.policiesBucket.Create(policyKVKey, policyData); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("store admin policy: %w", err)
	}

	// Create admin user (with policy already attached)
	adminUserID, err := generateIAMID("AIDA")
	if err != nil {
		return fmt.Errorf("generate admin user ID: %w", err)
	}
	kvKey := admin.AccountID + "." + admin.UserName
	adminUser := User{
		UserName:         admin.UserName,
		UserID:           adminUserID,
		AccountID:        admin.AccountID,
		ARN:              fmt.Sprintf("arn:aws:iam::%s:user/%s", admin.AccountID, admin.UserName),
		Path:             "/",
		CreatedAt:        now,
		AccessKeys:       []string{admin.AccessKeyID},
		Tags:             []Tag{},
		AttachedPolicies: []string{policyARN},
	}

	userData, err := json.Marshal(adminUser)
	if err != nil {
		return fmt.Errorf("marshal admin user: %w", err)
	}
	if _, err := s.usersBucket.Create(kvKey, userData); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("store admin user: %w", err)
	}

	// Create admin access key
	ak := AccessKey{
		AccessKeyID:     admin.AccessKeyID,
		SecretAccessKey: admin.EncryptedSecret,
		UserName:        admin.UserName,
		AccountID:       admin.AccountID,
		Status:          AccessKeyStatusActive,
		CreatedAt:       now,
	}
	akData, err := json.Marshal(ak)
	if err != nil {
		return fmt.Errorf("marshal admin access key: %w", err)
	}
	if err := s.createAccessKey(admin.AccessKeyID, akData); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return fmt.Errorf("store admin access key: %w", err)
	}

	slog.Info("Admin account seeded", "accountID", admin.AccountID, "userName", admin.UserName, "accessKeyID", admin.AccessKeyID)

	// Publish account creation event so the daemon creates a default VPC for
	// this account. Without this, the admin account has no default VPC/subnet
	// and the user must create one manually.
	if s.natsConn != nil {
		evt, err := json.Marshal(struct {
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
		}{AccountID: admin.AccountID, AccountName: admin.AccountName})
		if err != nil {
			slog.Warn("Failed to marshal account creation event", "accountID", admin.AccountID, "error", err)
			return nil
		}
		if err := s.natsConn.Publish("iam.account.created", evt); err != nil {
			slog.Warn("Failed to publish account creation event for admin account", "accountID", admin.AccountID, "error", err)
		}
	}

	return nil
}

// IsEmpty returns true if the users bucket has no entries.
func (s *IAMServiceImpl) IsEmpty() (bool, error) {
	keys, err := s.usersBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return true, nil
		}
		return false, fmt.Errorf("check users bucket: %w", err)
	}
	for _, key := range keys {
		if key != utils.VersionKey {
			return false, nil
		}
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Account Operations
// ---------------------------------------------------------------------------

// CreateAccount creates a new account with a sequentially assigned 12-digit ID.
// Uses a CAS loop on the counter bucket for safe concurrent ID assignment.
func (s *IAMServiceImpl) CreateAccount(name string) (*Account, error) {
	if name == "" {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	var accountID string
	for {
		// Read current counter value
		entry, err := s.accountCounterBucket.Get("next_id")
		if errors.Is(err, nats.ErrKeyNotFound) {
			// First account ever — start at 1 (000000000000 is the global account)
			accountID = fmt.Sprintf("%012d", 1)
			if _, err := s.accountCounterBucket.Create("next_id", []byte("2")); err != nil {
				if errors.Is(err, nats.ErrKeyExists) {
					continue // Another node raced us, retry
				}
				return nil, fmt.Errorf("init account counter: %w", err)
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read account counter: %w", err)
		}

		nextID, err := strconv.ParseInt(string(entry.Value()), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse account counter: %w", err)
		}
		accountID = fmt.Sprintf("%012d", nextID)

		// CAS update: increment counter, fail if revision changed
		newVal := []byte(strconv.FormatInt(nextID+1, 10))
		if _, err := s.accountCounterBucket.Update("next_id", newVal, entry.Revision()); err != nil {
			if errors.Is(err, nats.ErrKeyExists) {
				continue // CAS conflict, retry
			}
			return nil, fmt.Errorf("update account counter: %w", err)
		}
		break
	}

	account := Account{
		AccountID:   accountID,
		AccountName: name,
		Status:      AccountStatusActive,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(account)
	if err != nil {
		return nil, fmt.Errorf("marshal account: %w", err)
	}

	if _, err := s.accountsBucket.Create(accountID, data); err != nil {
		return nil, fmt.Errorf("store account: %w", err)
	}

	slog.Info("Account created", "accountID", accountID, "name", name)

	if s.natsConn != nil {
		evt, err := json.Marshal(struct {
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
		}{AccountID: accountID, AccountName: name})
		if err != nil {
			slog.Error("Failed to marshal account creation event", "accountID", accountID, "error", err)
		} else if err := s.natsConn.Publish("iam.account.created", evt); err != nil {
			slog.Error("Failed to publish account creation event", "accountID", accountID, "error", err)
		}
	}

	return &account, nil
}

// GetAccount retrieves an account by its 12-digit ID.
func (s *IAMServiceImpl) GetAccount(accountID string) (*Account, error) {
	entry, err := s.accountsBucket.Get(accountID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get account: %w", err)
	}

	var account Account
	if err := json.Unmarshal(entry.Value(), &account); err != nil {
		return nil, fmt.Errorf("unmarshal account: %w", err)
	}
	return &account, nil
}

// ListAccounts returns all accounts.
func (s *IAMServiceImpl) ListAccounts() ([]*Account, error) {
	keys, err := s.accountsBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return []*Account{}, nil
		}
		return nil, fmt.Errorf("list account keys: %w", err)
	}

	accounts := make([]*Account, 0, len(keys))
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := s.accountsBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("get account %s: %w", key, err)
		}

		var account Account
		if err := json.Unmarshal(entry.Value(), &account); err != nil {
			slog.Warn("ListAccounts: failed to unmarshal account", "key", key, "err", err)
			continue
		}
		accounts = append(accounts, &account)
	}
	return accounts, nil
}

// ---------------------------------------------------------------------------
// Policy CRUD
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) CreatePolicy(accountID string, input *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	policyName := *input.PolicyName
	if err := validatePolicyName(policyName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	kvKey := accountID + "." + policyName

	if _, err := ValidatePolicyDocument(*input.PolicyDocument); err != nil {
		slog.Debug("CreatePolicy: invalid policy document", "policyName", policyName, "err", err)
		return nil, errors.New(awserrors.ErrorIAMMalformedPolicyDocument)
	}

	path := aws.StringValue(input.Path)
	if path == "" {
		path = "/"
	} else if err := validatePath(path); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	newPolicyID, err := generateIAMID("ANPA")
	if err != nil {
		return nil, fmt.Errorf("generate policy ID: %w", err)
	}
	policy := Policy{
		PolicyName:     policyName,
		PolicyID:       newPolicyID,
		ARN:            fmt.Sprintf("arn:aws:iam::%s:policy%s%s", accountID, path, policyName),
		Path:           path,
		Description:    aws.StringValue(input.Description),
		PolicyDocument: *input.PolicyDocument,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		DefaultVersion: "v1",
		Tags:           []Tag{},
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal policy: %w", err)
	}

	if _, err := s.policiesBucket.Create(kvKey, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store policy: %w", err)
	}

	slog.Info("IAM policy created", "accountID", accountID, "policyName", policyName, "policyID", policy.PolicyID)

	createdAt := parseCreatedAt(policy.CreatedAt)
	return &iam.CreatePolicyOutput{
		Policy: &iam.Policy{
			PolicyName:       aws.String(policy.PolicyName),
			PolicyId:         aws.String(policy.PolicyID),
			Arn:              aws.String(policy.ARN),
			Path:             aws.String(policy.Path),
			Description:      aws.String(policy.Description),
			DefaultVersionId: aws.String(policy.DefaultVersion),
			CreateDate:       aws.Time(createdAt),
			AttachmentCount:  aws.Int64(0),
			IsAttachable:     aws.Bool(true),
		},
	}, nil
}

func (s *IAMServiceImpl) GetPolicy(accountID string, input *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	policy, err := s.getPolicyByARN(accountID, *input.PolicyArn)
	if err != nil {
		return nil, err
	}

	attachmentCount, err := s.countPolicyAttachments(accountID, policy.ARN)
	if err != nil {
		return nil, fmt.Errorf("check policy attachments: %w", err)
	}

	createdAt := parseCreatedAt(policy.CreatedAt)
	return &iam.GetPolicyOutput{
		Policy: &iam.Policy{
			PolicyName:       aws.String(policy.PolicyName),
			PolicyId:         aws.String(policy.PolicyID),
			Arn:              aws.String(policy.ARN),
			Path:             aws.String(policy.Path),
			Description:      aws.String(policy.Description),
			DefaultVersionId: aws.String(policy.DefaultVersion),
			CreateDate:       aws.Time(createdAt),
			AttachmentCount:  aws.Int64(attachmentCount),
			IsAttachable:     aws.Bool(true),
		},
	}, nil
}

func (s *IAMServiceImpl) GetPolicyVersion(accountID string, input *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	policy, err := s.getPolicyByARN(accountID, *input.PolicyArn)
	if err != nil {
		return nil, err
	}

	// We only support v1 — reject other version IDs
	if *input.VersionId != "v1" {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	createdAt := parseCreatedAt(policy.CreatedAt)
	return &iam.GetPolicyVersionOutput{
		PolicyVersion: &iam.PolicyVersion{
			Document:         aws.String(policy.PolicyDocument),
			VersionId:        aws.String("v1"),
			IsDefaultVersion: aws.Bool(true),
			CreateDate:       aws.Time(createdAt),
		},
	}, nil
}

func (s *IAMServiceImpl) ListPolicies(accountID string, input *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	keys, err := s.policiesBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return &iam.ListPoliciesOutput{
				Policies:    []*iam.Policy{},
				IsTruncated: aws.Bool(false),
			}, nil
		}
		return nil, fmt.Errorf("list policy keys: %w", err)
	}

	// Build attachment counts once for the whole account instead of per-policy.
	attachCounts, err := s.buildAttachmentCounts(accountID)
	if err != nil {
		return nil, fmt.Errorf("build attachment counts: %w", err)
	}

	keyPrefix := accountID + "."
	var policies []*iam.Policy
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.policiesBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("ListPolicies: policy key disappeared (concurrent delete)", "key", key)
			} else {
				slog.Warn("ListPolicies: failed to get policy", "key", key, "err", err)
			}
			continue
		}

		var policy Policy
		if err := json.Unmarshal(entry.Value(), &policy); err != nil {
			slog.Warn("ListPolicies: failed to unmarshal policy", "key", key, "err", err)
			continue
		}

		createdAt := parseCreatedAt(policy.CreatedAt)
		policies = append(policies, &iam.Policy{
			PolicyName:       aws.String(policy.PolicyName),
			PolicyId:         aws.String(policy.PolicyID),
			Arn:              aws.String(policy.ARN),
			Path:             aws.String(policy.Path),
			DefaultVersionId: aws.String(policy.DefaultVersion),
			CreateDate:       aws.Time(createdAt),
			AttachmentCount:  aws.Int64(attachCounts[policy.ARN]),
			IsAttachable:     aws.Bool(true),
		})
	}

	return &iam.ListPoliciesOutput{
		Policies:    policies,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeletePolicy(accountID string, input *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	policy, err := s.getPolicyByARN(accountID, *input.PolicyArn)
	if err != nil {
		return nil, err
	}

	attachCount, err := s.countPolicyAttachments(accountID, policy.ARN)
	if err != nil {
		return nil, fmt.Errorf("check policy attachments: %w", err)
	}
	if attachCount > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	kvKey := accountID + "." + policy.PolicyName
	if err := s.policiesBucket.Delete(kvKey); err != nil {
		return nil, fmt.Errorf("delete policy: %w", err)
	}

	slog.Info("IAM policy deleted", "accountID", accountID, "policyName", policy.PolicyName)
	return &iam.DeletePolicyOutput{}, nil
}

// ---------------------------------------------------------------------------
// Policy Attachment
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	userName := *input.UserName
	policyARN := *input.PolicyArn
	userKVKey := accountID + "." + userName

	// Verify policy exists
	if _, err := s.getPolicyByARN(accountID, policyARN); err != nil {
		return nil, err
	}

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	// Idempotent — if already attached, succeed silently
	if slices.Contains(user.AttachedPolicies, policyARN) {
		return &iam.AttachUserPolicyOutput{}, nil
	}

	user.AttachedPolicies = append(user.AttachedPolicies, policyARN)
	userData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	slog.Info("IAM policy attached to user", "accountID", accountID, "userName", userName, "policyArn", policyARN)
	return &iam.AttachUserPolicyOutput{}, nil
}

func (s *IAMServiceImpl) DetachUserPolicy(accountID string, input *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	userName := *input.UserName
	policyARN := *input.PolicyArn
	userKVKey := accountID + "." + userName

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	found := false
	remaining := make([]string, 0, len(user.AttachedPolicies))
	for _, arn := range user.AttachedPolicies {
		if arn == policyARN {
			found = true
		} else {
			remaining = append(remaining, arn)
		}
	}

	if !found {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	user.AttachedPolicies = remaining
	userData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	slog.Info("IAM policy detached from user", "accountID", accountID, "userName", userName, "policyArn", policyARN)
	return &iam.DetachUserPolicyOutput{}, nil
}

func (s *IAMServiceImpl) ListAttachedUserPolicies(accountID string, input *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	user, err := s.getUser(accountID, *input.UserName)
	if err != nil {
		return nil, err
	}

	var attached []*iam.AttachedPolicy
	for _, arn := range user.AttachedPolicies {
		policy, err := s.getPolicyByARN(accountID, arn)
		if err != nil {
			slog.Warn("ListAttachedUserPolicies: policy not found for ARN", "arn", arn, "err", err)
			continue
		}
		attached = append(attached, &iam.AttachedPolicy{
			PolicyArn:  aws.String(policy.ARN),
			PolicyName: aws.String(policy.PolicyName),
		})
	}

	return &iam.ListAttachedUserPoliciesOutput{
		AttachedPolicies: attached,
		IsTruncated:      aws.Bool(false),
	}, nil
}

// GetUserPolicies resolves all policy documents attached to a user.
// Used internally by the gateway for policy evaluation.
func (s *IAMServiceImpl) GetUserPolicies(accountID, userName string) ([]PolicyDocument, error) {
	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	var docs []PolicyDocument
	for _, arn := range user.AttachedPolicies {
		policy, err := s.getPolicyByARN(accountID, arn)
		if err != nil {
			// Fail closed: if we can't resolve a policy, we can't make a safe
			// access decision. Return an error so the caller denies access.
			return nil, fmt.Errorf("resolve policy %s: %w", arn, err)
		}

		// policy already validated at creation, so we skip validating again
		var doc PolicyDocument
		if err := json.Unmarshal([]byte(policy.PolicyDocument), &doc); err != nil {
			return nil, fmt.Errorf("parse policy %s: %w", policy.PolicyName, err)
		}
		docs = append(docs, doc)
	}

	return docs, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) getPolicyByARN(accountID, policyARN string) (*Policy, error) {
	// Extract policy name from ARN: arn:aws:iam::000000000000:policy/path/PolicyName
	parts := strings.SplitN(policyARN, ":policy", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	// The name is the last segment after the final /
	segments := strings.Split(parts[1], "/")
	policyName := segments[len(segments)-1]
	if policyName == "" {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	kvKey := accountID + "." + policyName
	entry, err := s.policiesBucket.Get(kvKey)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get policy: %w", err)
	}

	var policy Policy
	if err := json.Unmarshal(entry.Value(), &policy); err != nil {
		return nil, fmt.Errorf("unmarshal policy: %w", err)
	}

	// Verify the full ARN matches (path may differ)
	if policy.ARN != policyARN {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	return &policy, nil
}

// buildAttachmentCounts fetches all users for the account once and returns a
// map of policyARN -> number of users that have it attached. This avoids the
// N+1 pattern of calling countPolicyAttachments per policy in ListPolicies.
func (s *IAMServiceImpl) buildAttachmentCounts(accountID string) (map[string]int64, error) {
	keys, err := s.usersBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("build attachment counts: %w", err)
	}

	counts := make(map[string]int64)
	keyPrefix := accountID + "."
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.usersBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("buildAttachmentCounts: user key disappeared", "key", key)
				continue
			}
			slog.Warn("buildAttachmentCounts: failed to get user", "key", key, "err", err)
			continue
		}
		var user User
		if err := json.Unmarshal(entry.Value(), &user); err != nil {
			slog.Warn("buildAttachmentCounts: failed to unmarshal user", "key", key, "err", err)
			continue
		}
		for _, arn := range user.AttachedPolicies {
			counts[arn]++
		}
	}
	return counts, nil
}

// countPolicyAttachments counts how many users in this account have this policy attached.
func (s *IAMServiceImpl) countPolicyAttachments(accountID, policyARN string) (int64, error) {
	keys, err := s.usersBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("count policy attachments: %w", err)
	}

	keyPrefix := accountID + "."
	var count int64
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.usersBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("countPolicyAttachments: user key disappeared", "key", key)
				continue
			}
			slog.Warn("countPolicyAttachments: failed to get user", "key", key, "err", err)
			continue
		}
		var user User
		if err := json.Unmarshal(entry.Value(), &user); err != nil {
			slog.Warn("countPolicyAttachments: failed to unmarshal user", "key", key, "err", err)
			continue
		}
		if slices.Contains(user.AttachedPolicies, policyARN) {
			count++
		}
	}
	return count, nil
}

func (s *IAMServiceImpl) getUser(accountID, userName string) (*User, error) {
	kvKey := accountID + "." + userName
	entry, err := s.usersBucket.Get(kvKey)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get user: %w", err)
	}

	var user User
	if err := json.Unmarshal(entry.Value(), &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}
	return &user, nil
}

func generateIAMID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return prefix + strings.ToUpper(hex.EncodeToString(b))[:17], nil
}

func generateAccessKeyID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return LongLivedAccessKeyIDPrefix + strings.ToUpper(hex.EncodeToString(b)), nil
}

func parseCreatedAt(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		slog.Warn("parseCreatedAt: invalid RFC3339", "raw", raw, "err", err)
	}
	return t
}

const maxPolicyDocumentSize = 6144

// isIAMNameChar returns true if c is allowed in IAM user/policy names.
func isIAMNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '+' || c == '=' || c == ',' || c == '.' || c == '@' || c == '-' || c == '_'
}

func validateUserName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("user name must be between 1 and 64 characters")
	}
	for i := range len(name) {
		if !isIAMNameChar(name[i]) {
			return fmt.Errorf("user name contains invalid character: %q", name[i])
		}
	}
	return nil
}

func validatePolicyName(name string) error {
	if len(name) == 0 || len(name) > 128 {
		return fmt.Errorf("policy name must be between 1 and 128 characters")
	}
	for i := range len(name) {
		if !isIAMNameChar(name[i]) {
			return fmt.Errorf("policy name contains invalid character: %q", name[i])
		}
	}
	return nil
}

func validatePath(path string) error {
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") {
		return fmt.Errorf("path must begin and end with /")
	}
	if len(path) > 512 {
		return fmt.Errorf("path exceeds maximum length of 512")
	}
	return nil
}

// ValidatePolicyDocument parses and validates an IAM policy document JSON string.
func ValidatePolicyDocument(docJSON string) (*PolicyDocument, error) {
	if len(docJSON) > maxPolicyDocumentSize {
		return nil, fmt.Errorf("policy document exceeds maximum size of %d bytes", maxPolicyDocumentSize)
	}

	var doc PolicyDocument
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	if doc.Version != "2012-10-17" {
		return nil, fmt.Errorf("unsupported policy version: %q", doc.Version)
	}

	if len(doc.Statement) == 0 {
		return nil, fmt.Errorf("policy must contain at least one statement")
	}

	for i, stmt := range doc.Statement {
		if stmt.Effect != PolicyEffectAllow && stmt.Effect != PolicyEffectDeny {
			return nil, fmt.Errorf("statement %d: Effect must be Allow or Deny, got %q", i, stmt.Effect)
		}
		if len(stmt.Action) == 0 {
			return nil, fmt.Errorf("statement %d: Action is required", i)
		}
		if len(stmt.Resource) == 0 {
			return nil, fmt.Errorf("statement %d: Resource is required", i)
		}
	}

	return &doc, nil
}
