package handlers_ec2_key

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // G501: MD5 is the digest EC2 puts on the wire, not a security choice
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is the digest EC2 puts on the wire, not a security choice
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"golang.org/x/crypto/ssh"
)

// Ensure KeyServiceImpl implements KeyService.
var _ KeyService = (*KeyServiceImpl)(nil)

// KeyServiceImpl handles key pair operations with ssh-keygen and S3 storage.
type KeyServiceImpl struct {
	config     *config.Config
	store      objectstore.ObjectStore
	bucketName string
}

// NewKeyServiceImpl creates a new daemon-side key service.
func NewKeyServiceImpl(cfg *config.Config) *KeyServiceImpl {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	return &KeyServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: cfg.Predastore.Bucket,
	}
}

// NewKeyServiceImplWithStore creates a key service with a custom object store (for testing).
func NewKeyServiceImplWithStore(store objectstore.ObjectStore, bucketName string) *KeyServiceImpl {
	return &KeyServiceImpl{
		store:      store,
		bucketName: bucketName,
	}
}

// CreateKeyPair generates a new SSH key pair using ssh-keygen.
func (s *KeyServiceImpl) CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, accountID string) (*ec2.CreateKeyPairOutput, error) {
	if input == nil || input.KeyName == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	keyName := *input.KeyName
	slog.InfoContext(ctx, "Creating key pair", "keyName", keyName)

	// Validate key name contains only allowed characters
	if err := utils.ValidateKeyPairName(keyName); err != nil {
		slog.ErrorContext(ctx, "Invalid key pair name", "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidKeyPairFormat)
	}

	// Check if key already exists in S3
	keyPath := fmt.Sprintf("keys/%s/%s", accountID, keyName)
	_, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(keyPath),
	})

	if err == nil {
		// Object exists - return duplicate error
		slog.ErrorContext(ctx, "Key pair already exists", "keyName", keyName)
		return nil, errors.New(awserrors.ErrorInvalidKeyPairDuplicate)
	}

	// Determine key type (default: ed25519, optional: rsa)
	keyType := "ed25519"
	if input.KeyType != nil {
		switch *input.KeyType {
		case "rsa":
			keyType = "rsa"
		case "ed25519":
			keyType = "ed25519"
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	// Create temporary directory for key generation
	tmpDir, err := os.MkdirTemp("", "spinifex-keypair-*")
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create temp directory", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	defer os.RemoveAll(tmpDir)

	privateKeyPath := filepath.Join(tmpDir, "id_key")
	publicKeyPath := privateKeyPath + ".pub"

	// Generate key pair using ssh-keygen
	var cmd *exec.Cmd
	if keyType == "ed25519" {
		// ED25519 key (modern, recommended)
		cmd = exec.Command("ssh-keygen", "-t", "ed25519", "-f", privateKeyPath, "-N", "", "-C", "")
	} else {
		// RSA 2048-bit key
		cmd = exec.Command("ssh-keygen", "-t", "rsa", "-b", "2048", "-f", privateKeyPath, "-N", "", "-C", "")
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.ErrorContext(ctx, "ssh-keygen failed", "err", err, "stderr", stderr.String())
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Read private key
	privateKeyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to read private key", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Read public key
	publicKeyData, err := os.ReadFile(publicKeyPath)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to read public key", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Fingerprint the key we just generated; the digest algorithm follows the
	// key algorithm, so it is derived from the parsed key rather than keyType.
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicKeyData)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to parse generated public key", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	fingerprint, err := createdKeyFingerprint(privateKeyData, publicKey)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to fingerprint generated key pair", "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// From here keyType describes the key that exists rather than the one that
	// was asked for, so that the type stored alongside the fingerprint is read
	// off the same key the fingerprint was taken from. The import path types its
	// keys the same way. createdKeyFingerprint has already rejected every
	// algorithm this can refuse, so the error is unreachable and stated only so
	// an unsupported key can never be stored under a type EC2 cannot report.
	keyType, err = keyPairType(publicKey)
	if err != nil {
		slog.ErrorContext(ctx, "Generated key has an unsupported algorithm", "algorithm", publicKey.Type(), "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Upload public key to S3
	_, err = s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(keyPath),
		Body:   bytes.NewReader(publicKeyData),
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to upload public key to S3", "err", err, "path", keyPath)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Build response (similar to AWS EC2)
	keyPairID := utils.GenerateResourceID("key")
	tags := utils.MapToEC2Tags(utils.ExtractTags(input.TagSpecifications, "key-pair"))
	output := &ec2.CreateKeyPairOutput{
		KeyFingerprint: aws.String(fingerprint),
		KeyMaterial:    aws.String(string(privateKeyData)),
		KeyName:        aws.String(keyName),
		KeyPairId:      aws.String(keyPairID),
		Tags:           tags,
	}

	// Store metadata file (everything but the private key) for keyPairId lookups
	err = s.storeKeyPairMetadata(ctx, accountID, keyPairID, &keyPairMetadata{
		KeyFingerprint: aws.String(fingerprint),
		KeyName:        aws.String(keyName),
		KeyPairId:      aws.String(keyPairID),
		KeyType:        keyType,
		Tags:           tags,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to store key pair metadata", "err", err, "keyPairId", keyPairID)
		// Try to cleanup the public key we just uploaded
		if _, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(keyPath),
		}); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup public key", "err", err, "key", keyPath)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "Key pair created successfully", "keyName", keyName, "fingerprint", fingerprint, "keyPairId", keyPairID)

	return output, nil
}

// importedKeyFingerprint fingerprints a key supplied to ImportKeyPair: the MD5
// of the DER SubjectPublicKeyInfo for RSA, and the SHA-256 of the SSH wire blob
// for ED25519, both matching AWS.
func importedKeyFingerprint(publicKey ssh.PublicKey) (string, error) {
	switch publicKey.Type() {
	case ssh.KeyAlgoED25519:
		return ed25519Fingerprint(publicKey), nil

	case ssh.KeyAlgoRSA:
		// AWS hashes the DER SubjectPublicKeyInfo, not the RFC 4253 wire blob
		// that ssh.FingerprintLegacyMD5 uses, so unwrap to the crypto key and
		// re-encode.
		cryptoKey, ok := publicKey.(ssh.CryptoPublicKey)
		if !ok {
			return "", fmt.Errorf("key algorithm %q exposes no crypto public key", publicKey.Type())
		}
		der, err := x509.MarshalPKIXPublicKey(cryptoKey.CryptoPublicKey())
		if err != nil {
			return "", fmt.Errorf("marshal public key: %w", err)
		}

		sum := md5.Sum(der) //nolint:gosec // G401: MD5 is the digest EC2 puts on the wire, not a security choice
		return colonHex(sum[:]), nil

	default:
		// keyPairType rejects these first, so this is unreachable in the import
		// path. It is stated rather than left to x509, which happily marshals an
		// ECDSA key and would hand back a digest AWS has no counterpart for.
		return "", fmt.Errorf("unsupported key algorithm %q", publicKey.Type())
	}
}

// createdKeyFingerprint fingerprints a key EC2 generated itself. For RSA that is
// the SHA-1 of the DER PKCS#8 private key, a 20-byte digest over material the
// caller receives once and the store never keeps. ED25519 hashes the public key,
// exactly as the import path does.
func createdKeyFingerprint(privateKeyPEM []byte, publicKey ssh.PublicKey) (string, error) {
	switch publicKey.Type() {
	// Matched ahead of the PKCS#8 path deliberately: ParseRawPrivateKey hands
	// back *ed25519.PrivateKey, a pointer type MarshalPKCS8PrivateKey rejects,
	// so an ED25519 key reaching that path would fail at runtime.
	case ssh.KeyAlgoED25519:
		return ed25519Fingerprint(publicKey), nil

	case ssh.KeyAlgoRSA:
		rawKey, err := ssh.ParseRawPrivateKey(privateKeyPEM)
		if err != nil {
			return "", fmt.Errorf("parse generated private key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(rawKey)
		if err != nil {
			return "", fmt.Errorf("marshal private key: %w", err)
		}

		sum := sha1.Sum(der) //nolint:gosec // G401: SHA-1 is the digest EC2 puts on the wire, not a security choice
		return colonHex(sum[:]), nil

	default:
		return "", fmt.Errorf("unsupported key algorithm %q", publicKey.Type())
	}
}

// ed25519Fingerprint renders the SHA-256 of the SSH wire blob as EC2 spells it:
// bare padded base64, without the "SHA256:" prefix OpenSSH prepends and with the
// padding OpenSSH omits.
func ed25519Fingerprint(publicKey ssh.PublicKey) string {
	sum := sha256.Sum256(publicKey.Marshal())
	return base64.StdEncoding.EncodeToString(sum[:])
}

// colonHex renders digest bytes the way EC2 spells a fingerprint: lowercase hex
// byte pairs joined by colons.
func colonHex(digest []byte) string {
	encoded := hex.EncodeToString(digest)
	pairs := make([]string, 0, len(digest))
	for i := 0; i < len(encoded); i += 2 {
		pairs = append(pairs, encoded[i:i+2])
	}
	return strings.Join(pairs, ":")
}

// keyPairType maps an SSH public key algorithm to the EC2 key type string. EC2
// key pairs are RSA or ED25519 only, so every other algorithm -- ECDSA and
// ssh-dss included -- is rejected rather than stored under a type the API has no
// way to report back.
func keyPairType(publicKey ssh.PublicKey) (string, error) {
	switch publicKey.Type() {
	case ssh.KeyAlgoED25519:
		return "ed25519", nil
	case ssh.KeyAlgoRSA:
		return "rsa", nil
	default:
		return "", fmt.Errorf("unsupported key algorithm %q", publicKey.Type())
	}
}

// storeKeyPairMetadata stores key pair metadata (without private key) to S3 for keyPairId lookups.
func (s *KeyServiceImpl) storeKeyPairMetadata(ctx context.Context, accountID, keyPairID string, metadata *keyPairMetadata) error {
	// Store metadata with keyPairId as filename for efficient lookup when keyPairId is provided
	metadataPath := fmt.Sprintf("keys/%s/%s.json", accountID, keyPairID)

	// Marshal metadata to JSON
	jsonData, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Upload metadata to S3
	_, err = s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataPath),
		Body:   bytes.NewReader(jsonData),
	})
	if err != nil {
		return fmt.Errorf("failed to upload metadata: %w", err)
	}

	return nil
}

// getKeyNameFromKeyPairId retrieves the key name by directly reading the metadata file for a given keyPairId.
func (s *KeyServiceImpl) getKeyNameFromKeyPairId(ctx context.Context, accountID, keyPairID string) (string, error) {
	metadataPath := fmt.Sprintf("keys/%s/%s.json", accountID, keyPairID)

	// Get metadata from S3
	result, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataPath),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return "", errors.New(awserrors.ErrorInvalidKeyPairNotFound)
		}
		slog.Error("Failed to get key pair metadata", "keyPairId", keyPairID, "err", err)
		return "", fmt.Errorf("failed to get metadata: %w", err)
	}
	defer result.Body.Close()

	// Read and parse metadata
	body, err := io.ReadAll(result.Body)
	if err != nil {
		slog.Error("Failed to read metadata body", "err", err)
		return "", fmt.Errorf("failed to read metadata: %w", err)
	}

	metadata, err := decodeKeyPairMetadata(body)
	if err != nil {
		slog.Error("Failed to unmarshal metadata", "err", err)
		return "", fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	if metadata.KeyName == nil {
		slog.Error("Metadata missing KeyName field", "keyPairId", keyPairID)
		return "", fmt.Errorf("invalid metadata: missing KeyName")
	}

	return *metadata.KeyName, nil
}

// findKeyPairIdFromKeyName finds the keyPairId by searching metadata files for a given keyName.
func (s *KeyServiceImpl) findKeyPairIdFromKeyName(ctx context.Context, accountID, keyName string) (string, error) {
	prefix := fmt.Sprintf("keys/%s/", accountID)

	// List all objects with the keys prefix
	result, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucketName),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		slog.Error("Failed to list S3 objects", "prefix", prefix, "err", err)
		return "", fmt.Errorf("failed to list objects: %w", err)
	}

	// Check each .json metadata file
	for _, obj := range result.Contents {
		if obj.Key == nil {
			continue
		}

		// Only check .json files (metadata files)
		if !strings.HasSuffix(*obj.Key, ".json") {
			continue
		}

		// Get the metadata file
		// TODO: Have a more elegant solution, temporary until we have a proper key/value DB
		getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    obj.Key,
		})
		if err != nil {
			slog.Debug("Failed to get metadata file", "key", *obj.Key, "err", err)
			continue
		}

		body, err := io.ReadAll(getResult.Body)
		if err := getResult.Body.Close(); err != nil {
			slog.Debug("Failed to close metadata body", "key", *obj.Key, "err", err)
		}
		if err != nil {
			slog.Debug("Failed to read metadata body", "key", *obj.Key, "err", err)
			continue
		}

		metadata, err := decodeKeyPairMetadata(body)
		if err != nil {
			slog.Debug("Failed to unmarshal metadata", "key", *obj.Key, "err", err)
			continue
		}

		// Check if this metadata matches the keyName
		if metadata.KeyName != nil && *metadata.KeyName == keyName {
			if metadata.KeyPairId != nil {
				return *metadata.KeyPairId, nil
			}
		}
	}

	// Key pair not found
	return "", errors.New(awserrors.ErrorInvalidKeyPairNotFound)
}

// ValidateKeyPairExists checks if a key pair with the given name exists.
// Returns nil if the key pair exists, or an error with ErrorInvalidKeyPairNotFound if not.
func (s *KeyServiceImpl) ValidateKeyPairExists(ctx context.Context, accountID, keyName string) error {
	_, err := s.findKeyPairIdFromKeyName(ctx, accountID, keyName)
	return err
}

// GetPublicKeyMaterial returns the stored OpenSSH public key line, trimmed to a single
// line. NoSuchKey maps to ErrorInvalidKeyPairNotFound; other errors are returned as-is.
func (s *KeyServiceImpl) GetPublicKeyMaterial(accountID, keyName string) (string, error) {
	keyPath := fmt.Sprintf("keys/%s/%s", accountID, keyName)
	result, err := s.store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(keyPath),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return "", errors.New(awserrors.ErrorInvalidKeyPairNotFound)
		}
		slog.Error("Failed to get public key material", "keyName", keyName, "err", err)
		return "", fmt.Errorf("get public key %s: %w", keyPath, err)
	}
	defer result.Body.Close()

	body, err := io.ReadAll(result.Body)
	if err != nil {
		slog.Error("Failed to read public key material", "keyName", keyName, "err", err)
		return "", fmt.Errorf("read public key %s: %w", keyPath, err)
	}

	material := strings.TrimSpace(string(body))
	if material == "" {
		// An empty stored object is corruption, not "no key": surface a backend
		// error (→ 500) rather than claim definitive absence (→ 404 keyless boot).
		slog.Error("Empty public key material", "keyName", keyName, "path", keyPath)
		return "", fmt.Errorf("empty public key material at %s", keyPath)
	}
	return material, nil
}

// DeleteKeyPair removes a key pair (both public key and metadata from S3).
func (s *KeyServiceImpl) DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, accountID string) (*ec2.DeleteKeyPairOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	var keyName string
	var keyPairID string
	var err error

	// Determine keyName and keyPairId from input
	if input.KeyPairId != nil && *input.KeyPairId != "" {
		// KeyPairId provided - validate and look up keyName from metadata
		keyPairID = *input.KeyPairId

		// Validate keyPairId format (strip "key-" prefix before validation)
		keyPairIDStripped := strings.TrimPrefix(keyPairID, "key-")
		if err := utils.ValidateKeyPairName(keyPairIDStripped); err != nil {
			slog.ErrorContext(ctx, "Invalid key pair ID format", "keyPairId", keyPairID, "err", err)
			return nil, errors.New(awserrors.ErrorInvalidKeyPairFormat)
		}

		keyName, err = s.getKeyNameFromKeyPairId(ctx, accountID, keyPairID)
		if err != nil {
			// AWS DeleteKeyPair is idempotent — return success for non-existent keys
			if err.Error() == awserrors.ErrorInvalidKeyPairNotFound {
				slog.DebugContext(ctx, "DeleteKeyPair: key pair not found, returning success (idempotent)", "keyPairId", keyPairID)
				return &ec2.DeleteKeyPairOutput{}, nil
			}
			slog.ErrorContext(ctx, "Failed to get keyName from keyPairId", "keyPairId", keyPairID, "err", err)
			return nil, err
		}
	} else if input.KeyName != nil && *input.KeyName != "" {
		// KeyName provided - validate and find the keyPairId
		keyName = *input.KeyName

		// Validate keyName format
		if err := utils.ValidateKeyPairName(keyName); err != nil {
			slog.ErrorContext(ctx, "Invalid key pair name format", "keyName", keyName, "err", err)
			return nil, errors.New(awserrors.ErrorInvalidKeyPairFormat)
		}

		keyPairID, err = s.findKeyPairIdFromKeyName(ctx, accountID, keyName)
		if err != nil {
			// AWS DeleteKeyPair is idempotent — return success for non-existent keys
			if err.Error() == awserrors.ErrorInvalidKeyPairNotFound {
				slog.DebugContext(ctx, "DeleteKeyPair: key pair not found, returning success (idempotent)", "keyName", keyName)
				return &ec2.DeleteKeyPairOutput{}, nil
			}
			slog.ErrorContext(ctx, "Failed to find keyPairId from keyName", "keyName", keyName, "err", err)
			return nil, err
		}
	} else {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	slog.InfoContext(ctx, "Deleting key pair", "keyName", keyName, "keyPairId", keyPairID)

	// Delete public key
	publicKeyPath := fmt.Sprintf("keys/%s/%s", accountID, keyName)
	_, err = s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(publicKeyPath),
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to delete public key", "path", publicKeyPath, "err", err)
		// Continue to try deleting metadata even if public key deletion fails
	}

	// Delete metadata file (stored with keyPairID)
	metadataPath := fmt.Sprintf("keys/%s/%s.json", accountID, keyPairID)
	_, err = s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataPath),
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to delete metadata", "path", metadataPath, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "Key pair deleted successfully", "keyName", keyName, "keyPairId", keyPairID)

	return &ec2.DeleteKeyPairOutput{}, nil
}

// DescribeKeyPairs lists available key pairs by reading metadata files from S3
// describeKeyPairsValidFilters defines the set of filter names accepted by DescribeKeyPairs.
var describeKeyPairsValidFilters = map[string]bool{
	"key-pair-id": true,
	"key-name":    true,
	"fingerprint": true,
}

func (s *KeyServiceImpl) DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, accountID string) (*ec2.DescribeKeyPairsOutput, error) {
	if input == nil {
		input = &ec2.DescribeKeyPairsInput{}
	}

	slog.InfoContext(ctx, "Describing key pairs", "filters", input.Filters)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeKeyPairsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeKeyPairs: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := fmt.Sprintf("keys/%s/", accountID)

	// List all objects with the keys prefix
	result, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucketName),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to list S3 objects", "prefix", prefix, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Extract key-pair-id filter values for early S3 skip. The metadata
	// filename is keys/<account>/<keyPairID>.json, so we can match the ID
	// from the path before fetching the object.
	var keyPairIDFilterValues []string
	if parsedFilters != nil {
		keyPairIDFilterValues = parsedFilters["key-pair-id"]
	}

	var keyPairs []*ec2.KeyPairInfo

	// Check each .json metadata file
	for _, obj := range result.Contents {
		if obj.Key == nil {
			continue
		}

		// Only check .json files (metadata files)
		if !strings.HasSuffix(*obj.Key, ".json") {
			continue
		}

		// Early skip: if key-pair-id filter is set, derive the ID from the
		// S3 object path (keys/<account>/<keyPairID>.json) before fetching.
		if len(keyPairIDFilterValues) > 0 {
			objKey := *obj.Key
			kpID := strings.TrimSuffix(strings.TrimPrefix(objKey, prefix), ".json")
			if !filterutil.MatchesAny(keyPairIDFilterValues, kpID) {
				continue
			}
		}

		// Get the metadata file
		getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    obj.Key,
		})
		if err != nil {
			slog.DebugContext(ctx, "Failed to get metadata file", "key", *obj.Key, "err", err)
			continue
		}

		body, err := io.ReadAll(getResult.Body)
		if err := getResult.Body.Close(); err != nil {
			slog.DebugContext(ctx, "Failed to close metadata body", "key", *obj.Key, "err", err)
		}
		if err != nil {
			slog.DebugContext(ctx, "Failed to read metadata body", "key", *obj.Key, "err", err)
			continue
		}

		metadata, err := decodeKeyPairMetadata(body)
		if err != nil {
			slog.DebugContext(ctx, "Failed to unmarshal metadata", "key", *obj.Key, "err", err)
			continue
		}

		// Filter by KeyName if specified
		if len(input.KeyNames) > 0 {
			found := false
			for _, filterName := range input.KeyNames {
				if filterName != nil && metadata.KeyName != nil && *filterName == *metadata.KeyName {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Filter by KeyPairId if specified
		if len(input.KeyPairIds) > 0 {
			found := false
			for _, filterID := range input.KeyPairIds {
				if filterID != nil && metadata.KeyPairId != nil && *filterID == *metadata.KeyPairId {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Build KeyPairInfo from metadata. Records predating the stored KeyType
		// were typed by the decoder, which also normalised the fingerprint they
		// were typed from; the object itself is left as it was found, so a list
		// does not turn into a write per key pair.
		keyPairInfo := &ec2.KeyPairInfo{
			KeyPairId:      metadata.KeyPairId,
			KeyFingerprint: metadata.KeyFingerprint,
			KeyName:        metadata.KeyName,
			KeyType:        aws.String(metadata.KeyType),
			Tags:           metadata.Tags,
		}

		// Use S3 object LastModified as CreateTime
		if obj.LastModified != nil {
			keyPairInfo.CreateTime = obj.LastModified
		}

		// Apply filters
		if len(parsedFilters) > 0 && !keyPairMatchesFilters(keyPairInfo, parsedFilters) {
			continue
		}

		keyPairs = append(keyPairs, keyPairInfo)
	}

	slog.InfoContext(ctx, "DescribeKeyPairs completed", "count", len(keyPairs))

	return &ec2.DescribeKeyPairsOutput{
		KeyPairs: keyPairs,
	}, nil
}

// keyPairMatchesFilters checks whether a KeyPairInfo satisfies all parsed filters.
func keyPairMatchesFilters(kp *ec2.KeyPairInfo, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "key-pair-id":
			if kp.KeyPairId != nil {
				field = *kp.KeyPairId
			}
		case "key-name":
			if kp.KeyName != nil {
				field = *kp.KeyName
			}
		case "fingerprint":
			if kp.KeyFingerprint != nil {
				field = *kp.KeyFingerprint
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	// Check tag:Key filters
	tags := filterutil.EC2TagsToMap(kp.Tags)
	return filterutil.MatchesTags(filters, tags)
}

// ImportKeyPair imports an existing public key.
func (s *KeyServiceImpl) ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, accountID string) (*ec2.ImportKeyPairOutput, error) {
	if input == nil || input.KeyName == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	if len(input.PublicKeyMaterial) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	keyName := *input.KeyName
	slog.InfoContext(ctx, "Importing key pair", "keyName", keyName)

	// Validate key name contains only allowed characters
	if err := utils.ValidateKeyPairName(keyName); err != nil {
		slog.ErrorContext(ctx, "Invalid key pair name", "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidKeyPairFormat)
	}

	// Check if key already exists in S3
	keyPath := fmt.Sprintf("keys/%s/%s", accountID, keyName)
	_, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(keyPath),
	})

	if err == nil {
		// Object exists - return duplicate error
		slog.ErrorContext(ctx, "Key pair already exists", "keyName", keyName)
		return nil, errors.New(awserrors.ErrorInvalidKeyPairDuplicate)
	}

	// The material is stored verbatim and later served to instances as their
	// authorized_keys, so it must hold exactly the one key the returned
	// fingerprint describes. ParseAuthorizedKey skips leading comment and junk
	// lines and stops at the first key, so a multi-line blob would otherwise be
	// stored -- and trusted by the guest -- in full while only its first key was
	// validated. Requiring a single line is how that rule is enforced; RFC 4716
	// material is multi-line and would need normalising to an OpenSSH line
	// before it reached here.
	//
	// This binds new imports only. Records written before the check exists are
	// never revalidated, so material already in the object store may still be
	// multi-line or option-prefixed and is still served to guests as-is.
	publicKeyData := bytes.TrimSpace(input.PublicKeyMaterial)
	if bytes.ContainsAny(publicKeyData, "\r\n") {
		slog.ErrorContext(ctx, "Public key material is not a single key", "keyName", keyName)
		return nil, errors.New(awserrors.ErrorInvalidKeyFormat)
	}

	// Parse the authorized-key line ("ssh-rsa AAAAB... comment"), which also
	// validates the base64 body against the algorithm's wire encoding.
	publicKey, _, options, _, err := ssh.ParseAuthorizedKey(publicKeyData)
	if err != nil {
		slog.ErrorContext(ctx, "Invalid public key format", "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidKeyFormat)
	}

	// An option prefix ("command=...", "from=...") is not covered by the
	// fingerprint, yet sshd would apply it to every login on every instance
	// launched with this key pair. Refuse to import access the API cannot report.
	if len(options) > 0 {
		slog.ErrorContext(ctx, "Public key material carries authorized_keys options", "keyName", keyName, "options", options)
		return nil, errors.New(awserrors.ErrorInvalidKeyFormat)
	}

	keyType, err := keyPairType(publicKey)
	if err != nil {
		slog.ErrorContext(ctx, "Unsupported key type", "algorithm", publicKey.Type(), "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidKeyFormat)
	}

	fingerprint, err := importedKeyFingerprint(publicKey)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to fingerprint imported key", "keyName", keyName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Upload public key to S3
	_, err = s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(keyPath),
		Body:   bytes.NewReader(publicKeyData),
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to upload public key to S3", "err", err, "path", keyPath)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Generate key pair ID
	keyPairID := utils.GenerateResourceID("key")

	// Build response output
	tags := utils.MapToEC2Tags(utils.ExtractTags(input.TagSpecifications, "key-pair"))
	output := &ec2.ImportKeyPairOutput{
		KeyFingerprint: aws.String(fingerprint),
		KeyName:        aws.String(keyName),
		KeyPairId:      aws.String(keyPairID),
		Tags:           tags,
	}

	// Store metadata file (without public key material)
	err = s.storeKeyPairMetadata(ctx, accountID, keyPairID, &keyPairMetadata{
		KeyFingerprint: aws.String(fingerprint),
		KeyName:        aws.String(keyName),
		KeyPairId:      aws.String(keyPairID),
		KeyType:        keyType,
		Tags:           tags,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to store key pair metadata", "err", err, "keyPairId", keyPairID)
		// Try to cleanup the public key we just uploaded
		if _, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(keyPath),
		}); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup public key", "err", err, "key", keyPath)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "Key pair imported successfully", "keyName", keyName, "fingerprint", fingerprint, "keyPairId", keyPairID, "keyType", keyType)

	return output, nil
}

// ApplyRecordTags mirrors CreateTags into the owning key-pair metadata so
// DescribeKeyPairs observes tags added after create. Non-key ids and key pairs
// absent from the caller's account prefix are skipped.
func (s *KeyServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorKeyPairTags(context.Background(), input.Resources, accountID, utils.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning key-pair metadata with
// AWS-faithful delete semantics.
func (s *KeyServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorKeyPairTags(context.Background(), input.Resources, accountID, utils.RemoveTagsMut(input))
}

// mirrorKeyPairTags read-modify-writes the metadata Tags slice for each key-
// id, converting through a map to reuse the shared merge/remove semantics.
// Metadata is stored under the caller's account prefix, so a cross-account or
// absent key pair simply misses and no-ops.
func (s *KeyServiceImpl) mirrorKeyPairTags(ctx context.Context, resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "key-") {
			continue
		}
		metadataPath := fmt.Sprintf("keys/%s/%s.json", accountID, *res)
		result, err := s.store.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(metadataPath),
		})
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				continue
			}
			return fmt.Errorf("failed to get key pair metadata: %w", err)
		}
		body, err := io.ReadAll(result.Body)
		result.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to read key pair metadata: %w", err)
		}
		// The whole record is rewritten, not just its Tags, so a legacy record is
		// persisted here in the form the decoder upgraded it to.
		metadata, err := decodeKeyPairMetadata(body)
		if err != nil {
			return fmt.Errorf("failed to unmarshal key pair metadata: %w", err)
		}
		tags := filterutil.EC2TagsToMap(metadata.Tags)
		if tags == nil {
			tags = map[string]string{}
		}
		mut(tags)
		metadata.Tags = utils.MapToEC2Tags(tags)
		if err := s.storeKeyPairMetadata(ctx, accountID, *res, metadata); err != nil {
			return err
		}
	}
	return nil
}
