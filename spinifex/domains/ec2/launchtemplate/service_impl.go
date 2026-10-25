package launchtemplate

import (
	"context"
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
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure LaunchTemplateServiceImpl implements LaunchTemplateService.
var _ LaunchTemplateService = (*LaunchTemplateServiceImpl)(nil)

const (
	KVBucketLaunchTemplates        = "spinifex-launch-templates"
	KVBucketLaunchTemplatesVersion = 1

	// launchTemplateTagResourceType is the TagSpecification.ResourceType that tags
	// the template itself (as opposed to the instances it later launches).
	launchTemplateTagResourceType = "launch-template"

	// maxVersionRetries bounds the optimistic version-number kv.Create retry loop.
	maxVersionRetries = 8

	// versionDefault and versionLatest are the AWS version aliases.
	versionDefault = "$Default"
	versionLatest  = "$Latest"
)

// LaunchTemplateServiceImpl implements launch template operations with NATS
// JetStream persistence. Templates use three key kinds, all account-scoped:
// header (account.lt-<id>), name index (account.name.<name> -> lt-<id>), and
// immutable version bodies (account.lt-<id>.v<n>).
type LaunchTemplateServiceImpl struct {
	config   *config.Config
	natsConn *nats.Conn
	kv       jetstream.KeyValue

	// Optional: nil leaves the central tag index untouched by delete.
	centralTags CentralTagStore
}

// NewLaunchTemplateServiceImplWithNATS creates a launch template service with NATS JetStream.
func NewLaunchTemplateServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*LaunchTemplateServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketLaunchTemplates, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketLaunchTemplates, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketLaunchTemplates, kv, KVBucketLaunchTemplatesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketLaunchTemplates, err)
	}

	slog.Info("Launch template service initialized with JetStream KV", "bucket", KVBucketLaunchTemplates)

	return &LaunchTemplateServiceImpl{
		config:   cfg,
		natsConn: natsConn,
		kv:       kv,
	}, nil
}

// --- key helpers ---

// nameKey returns the name-index key: account.name.<hex-encoded-name>.
// Launch template names allow characters such as parentheses that are not valid
// in NATS KV keys, so the name must not be used in its raw form here.
func nameKey(accountID, name string) string {
	return kvutil.AccountKey(accountID, "name."+hex.EncodeToString([]byte(name)))
}

// versionKey returns a version-body key: account.lt-<id>.v<n>.
func versionKey(accountID, ltID string, n int64) string {
	return kvutil.AccountKey(accountID, ltID+".v"+strconv.FormatInt(n, 10))
}

// versionPrefix returns the scan prefix for a template's version bodies.
func versionPrefix(accountID, ltID string) string {
	return kvutil.AccountKey(accountID, ltID+".v")
}

// --- record load helpers ---

// getHeaderByID reads a header and its KV entry (for CAS) by launch template id.
func (s *LaunchTemplateServiceImpl) getHeaderByID(ctx context.Context, accountID, ltID string) (*LaunchTemplateHeader, jetstream.KeyValueEntry, error) {
	entry, err := s.kv.Get(ctx, kvutil.AccountKey(accountID, ltID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateIdNotFound)
		}
		// A header the store cannot return now is not proven absent.
		slog.ErrorContext(ctx, "Failed to read launch template header", "launchTemplateId", ltID, "err", err)
		return nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	var h LaunchTemplateHeader
	if err := json.Unmarshal(entry.Value(), &h); err != nil {
		return nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &h, entry, nil
}

// getHeaderByName resolves a template name to its header via the name index.
// A dangling name index (header missing) is treated as not-found.
func (s *LaunchTemplateServiceImpl) getHeaderByName(ctx context.Context, accountID, name string) (*LaunchTemplateHeader, jetstream.KeyValueEntry, error) {
	entry, err := s.kv.Get(ctx, nameKey(accountID, name))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException)
		}
		slog.ErrorContext(ctx, "Failed to read launch template name index", "err", err)
		return nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	ltID := string(entry.Value())
	h, hentry, err := s.getHeaderByID(ctx, accountID, ltID)
	if err != nil {
		if err.Error() == awserrors.ErrorInvalidLaunchTemplateIdNotFound {
			return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException)
		}
		return nil, nil, err
	}
	return h, hentry, nil
}

// resolveHeader resolves a template by id or name (mutually exclusive) and
// returns the header plus its KV entry for CAS operations.
func (s *LaunchTemplateServiceImpl) resolveHeader(ctx context.Context, accountID string, ltID, name *string) (*LaunchTemplateHeader, jetstream.KeyValueEntry, error) {
	id := aws.StringValue(ltID)
	nm := aws.StringValue(name)
	switch {
	case id != "" && nm != "":
		return nil, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	case id != "":
		if err := validateTemplateID(id); err != nil {
			return nil, nil, err
		}
		h, entry, err := s.getHeaderByID(ctx, accountID, id)
		if err != nil && err.Error() == awserrors.ErrorInvalidLaunchTemplateIdNotFound {
			return nil, nil, awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateIdNotFound,
				"The specified launch template, with template ID %s, does not exist.", id)
		}
		return h, entry, err
	case nm != "":
		h, entry, err := s.getHeaderByName(ctx, accountID, nm)
		if err != nil && err.Error() == awserrors.ErrorInvalidLaunchTemplateNameNotFoundException {
			return nil, nil, awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException,
				"The specified launch template, with template name %s, does not exist.", nm)
		}
		return h, entry, err
	default:
		return nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}
}

// getVersion loads an immutable version body. A missing body is the read-side
// guard that turns a dangling default or deleted version into VersionNotFound.
func (s *LaunchTemplateServiceImpl) getVersion(ctx context.Context, accountID, ltID string, n int64) (*LaunchTemplateVersionRec, error) {
	entry, err := s.kv.Get(ctx, versionKey(accountID, ltID, n))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound)
	}
	var rec LaunchTemplateVersionRec
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &rec, nil
}

// listVersionNumbers returns the existing version numbers for a template,
// derived by scanning its version keys (sorted ascending).
func (s *LaunchTemplateServiceImpl) listVersionNumbers(ctx context.Context, accountID, ltID string) ([]int64, error) {
	keys, err := s.kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	prefix := versionPrefix(accountID, ltID)
	var nums []int64
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		n, err := strconv.ParseInt(k[len(prefix):], 10, 64)
		if err != nil {
			continue
		}
		nums = append(nums, n)
	}
	slices.Sort(nums)
	return nums, nil
}

// latestVersionNumber returns the highest existing version number, or 0 if none.
func (s *LaunchTemplateServiceImpl) latestVersionNumber(ctx context.Context, accountID, ltID string) (int64, error) {
	nums, err := s.listVersionNumbers(ctx, accountID, ltID)
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, n := range nums {
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}

// latestVersionsFromKeys derives the highest existing version number per template
// from a single bucket key listing (all keys under prefix account.), avoiding a
// per-template rescan. Version keys have the shape account.lt-<id>.v<n>.
func latestVersionsFromKeys(keys []string, prefix string) map[string]int64 {
	latest := make(map[string]int64)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if !strings.HasPrefix(rest, "lt-") {
			continue
		}
		id, vStr, found := strings.Cut(rest, ".v")
		if !found {
			continue
		}
		n, err := strconv.ParseInt(vStr, 10, 64)
		if err != nil {
			continue
		}
		if n > latest[id] {
			latest[id] = n
		}
	}
	return latest
}

// resolveVersionNumber maps a selector ("", $Default, $Latest, or numeric) to a
// concrete version number. It does not verify the body exists — callers load the
// body and surface VersionNotFound when it is missing.
func (s *LaunchTemplateServiceImpl) resolveVersionNumber(ctx context.Context, accountID string, h *LaunchTemplateHeader, sel string) (int64, error) {
	switch sel {
	case "", versionDefault:
		return h.DefaultVersionNumber, nil
	case versionLatest:
		return s.latestVersionNumber(ctx, accountID, h.LaunchTemplateId)
	default:
		n, err := strconv.ParseInt(sel, 10, 64)
		if err != nil {
			return 0, errors.New(awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound)
		}
		return n, nil
	}
}

// --- CreateLaunchTemplate ---

// CreateLaunchTemplate implements the EC2 action. ClientToken is accepted but not stored: claimName
// reserves the name with an atomic Create, so a duplicate request cannot make a second template. It
// gets AlreadyExists where AWS would replay the first response.
func (s *LaunchTemplateServiceImpl) CreateLaunchTemplate(ctx context.Context, input *ec2.CreateLaunchTemplateInput, accountID string) (*ec2.CreateLaunchTemplateOutput, error) {
	if input.LaunchTemplateData == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	name := aws.StringValue(input.LaunchTemplateName)
	if err := validateTemplateName(name); err != nil {
		return nil, err
	}

	data, err := requestToResponse(input.LaunchTemplateData)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	ltID := awsidentifiers.GenerateResourceID("lt")
	if err := s.claimName(ctx, accountID, name, ltID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rec := LaunchTemplateVersionRec{
		VersionNumber:      1,
		VersionDescription: aws.StringValue(input.VersionDescription),
		CreateTime:         now,
		CreatedBy:          accountID,
		Data:               data,
	}
	if err := s.putVersion(ctx, accountID, ltID, &rec); err != nil {
		s.abandonCreate(ctx, accountID, name, ltID, false)
		return nil, err
	}

	header := LaunchTemplateHeader{
		LaunchTemplateId:     ltID,
		LaunchTemplateName:   name,
		AccountID:            accountID,
		CreatedBy:            accountID,
		CreateTime:           now,
		DefaultVersionNumber: 1,
		Tags:                 awstags.Extract(input.TagSpecifications, launchTemplateTagResourceType),
	}
	if err := s.putHeader(ctx, accountID, &header); err != nil {
		s.abandonCreate(ctx, accountID, name, ltID, true)
		return nil, err
	}

	slog.InfoContext(ctx, "CreateLaunchTemplate completed", "launchTemplateId", ltID, "name", name, "accountID", accountID)

	return &ec2.CreateLaunchTemplateOutput{
		LaunchTemplate: headerToEC2(&header, 1),
	}, nil
}

// abandonCreate removes what a failed create wrote, newest first. A failure
// is logged; what remains has no header, and the account cleanup removes it
// once it is older than an in-flight create could be.
func (s *LaunchTemplateServiceImpl) abandonCreate(ctx context.Context, accountID, name, ltID string, wroteVersion bool) {
	if wroteVersion {
		if err := s.kv.Delete(ctx, versionKey(accountID, ltID, 1)); err != nil {
			slog.ErrorContext(ctx, "CreateLaunchTemplate: failed to remove version 1 after a failed create", "launchTemplateId", ltID, "err", err)
		}
	}
	if err := s.releaseName(ctx, accountID, name, ltID); err != nil {
		slog.ErrorContext(ctx, "CreateLaunchTemplate: failed to release the name after a failed create", "launchTemplateId", ltID, "err", err)
	}
}

// releaseName removes the name index only while it still names ltID, at the
// revision read, so a name another template now holds is never released.
func (s *LaunchTemplateServiceImpl) releaseName(ctx context.Context, accountID, name, ltID string) error {
	key := nameKey(accountID, name)
	entry, err := s.kv.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("read name index: %w", err)
	case string(entry.Value()) != ltID:
		return nil
	}
	if err := s.kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
		return fmt.Errorf("delete name index: %w", err)
	}
	return nil
}

// errNameInUse is AWS's error for a launch template name already taken.
func errNameInUse() error {
	return awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateNameAlreadyExistsException, "Launch template name already in use.")
}

// claimName atomically reserves a template name via kv.Create, with
// repair-on-write: if the name already exists but its header is gone (a crash
// orphan), reclaim it via a revision-guarded CAS update. Only a name whose header
// still exists returns AlreadyExists.
func (s *LaunchTemplateServiceImpl) claimName(ctx context.Context, accountID, name, ltID string) error {
	key := nameKey(accountID, name)
	if _, err := s.kv.Create(ctx, key, []byte(ltID)); err == nil {
		return nil
	}
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	// Only a genuinely missing header is a crash orphan safe to reclaim. A live
	// header means the name is taken; any other read error (transient fault, or a
	// concurrent in-flight create whose header is not yet written) fails closed so
	// the name is never stolen from a possibly-live template.
	_, herr := s.kv.Get(ctx, kvutil.AccountKey(accountID, string(entry.Value())))
	switch {
	case herr == nil:
		return errNameInUse()
	case !errors.Is(herr, jetstream.ErrKeyNotFound):
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Update(ctx, key, []byte(ltID), entry.Revision()); err != nil {
		return errNameInUse()
	}
	return nil
}

func (s *LaunchTemplateServiceImpl) putHeader(ctx context.Context, accountID string, h *LaunchTemplateHeader) error {
	data, err := json.Marshal(h)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Put(ctx, kvutil.AccountKey(accountID, h.LaunchTemplateId), data); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

func (s *LaunchTemplateServiceImpl) putVersion(ctx context.Context, accountID, ltID string, rec *LaunchTemplateVersionRec) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Put(ctx, versionKey(accountID, ltID, rec.VersionNumber), data); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// --- CreateLaunchTemplateVersion ---

func (s *LaunchTemplateServiceImpl) CreateLaunchTemplateVersion(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, accountID string) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	if input.LaunchTemplateData == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	header, entry, err := s.resolveHeader(ctx, accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}

	override, err := requestToResponse(input.LaunchTemplateData)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// SourceVersion clones-and-overrides; without it the new version is the data as given.
	data := override
	if src := aws.StringValue(input.SourceVersion); src != "" {
		n, err := s.resolveVersionNumber(ctx, accountID, header, src)
		if err != nil {
			return nil, withVersionNotFoundMessage(err, "Launch template version does not exist.")
		}
		base, err := s.getVersion(ctx, accountID, header.LaunchTemplateId, n)
		if err != nil {
			return nil, withVersionNotFoundMessage(err, "Launch template version does not exist.")
		}
		data = mergeResponseData(base.Data, override)
	}

	rec := LaunchTemplateVersionRec{
		VersionDescription: aws.StringValue(input.VersionDescription),
		CreateTime:         time.Now().UTC(),
		CreatedBy:          accountID,
		Data:               data,
	}

	// Assign the number with an atomic kv.Create in a bounded retry loop: on
	// collision another writer took n, so rescan and try the next number.
	for attempt := range maxVersionRetries {
		latest, err := s.latestVersionNumber(ctx, accountID, header.LaunchTemplateId)
		if err != nil {
			return nil, err
		}
		next := latest + 1
		rec.VersionNumber = next
		body, err := json.Marshal(&rec)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if _, err := s.kv.Create(ctx, versionKey(accountID, header.LaunchTemplateId, next), body); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				slog.DebugContext(ctx, "CreateLaunchTemplateVersion: version-number collision, retrying", "attempt", attempt, "next", next)
				continue
			}
			slog.ErrorContext(ctx, "CreateLaunchTemplateVersion: version-key create failed", "next", next, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if err := s.commitVersion(ctx, accountID, header.LaunchTemplateId, entry); err != nil {
			s.abandonVersion(ctx, accountID, header.LaunchTemplateId, next)
			if errors.Is(err, errTemplateGone) {
				return nil, templateNotFound(input.LaunchTemplateId, input.LaunchTemplateName)
			}
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		slog.InfoContext(ctx, "CreateLaunchTemplateVersion completed", "launchTemplateId", header.LaunchTemplateId, "version", next, "accountID", accountID)
		return &ec2.CreateLaunchTemplateVersionOutput{
			LaunchTemplateVersion: versionToEC2(header, &rec),
		}, nil
	}

	slog.ErrorContext(ctx, "CreateLaunchTemplateVersion: version-number retries exhausted", "launchTemplateId", header.LaunchTemplateId, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// errTemplateGone: the header was deleted while a version was being added.
var errTemplateGone = errors.New("launch template deleted")

// commitVersion rewrites the header's current value under a revision guard
// after a new body is written. A delete that removed the header at any earlier
// revision makes it fail, so the body is never left without its template.
func (s *LaunchTemplateServiceImpl) commitVersion(ctx context.Context, accountID, ltID string, entry jetstream.KeyValueEntry) error {
	return s.commitHeader(ctx, "CreateLaunchTemplateVersion", accountID, ltID, entry, nil)
}

// commitHeader rewrites the header's current value at the revision read,
// re-reading on a conflict. A non-nil check vets each re-read header and
// stops the commit with its error.
func (s *LaunchTemplateServiceImpl) commitHeader(ctx context.Context, op, accountID, ltID string, entry jetstream.KeyValueEntry, check func(*LaunchTemplateHeader) error) error {
	key := kvutil.AccountKey(accountID, ltID)
	for range maxVersionRetries {
		_, err := s.kv.Update(ctx, key, entry.Value(), entry.Revision())
		if err == nil {
			return nil
		}
		if !isRevisionConflict(err) {
			slog.ErrorContext(ctx, op+": header commit failed", "launchTemplateId", ltID, "err", err)
			return err
		}
		next, gerr := s.kv.Get(ctx, key)
		if errors.Is(gerr, jetstream.ErrKeyNotFound) {
			return errTemplateGone
		}
		if gerr != nil {
			return gerr
		}
		if check != nil {
			var h LaunchTemplateHeader
			if err := json.Unmarshal(next.Value(), &h); err != nil {
				return fmt.Errorf("decode header: %w", err)
			}
			if err := check(&h); err != nil {
				return err
			}
		}
		entry = next
	}
	return errors.New("header commit retries exhausted")
}

// isRevisionConflict reports a CAS write that lost to a concurrent writer.
func isRevisionConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists)
}

// abandonVersion removes a body whose header commit failed. A failure is
// logged and leaves a headerless body, which no ID, name or launch resolves.
func (s *LaunchTemplateServiceImpl) abandonVersion(ctx context.Context, accountID, ltID string, n int64) {
	if err := s.kv.Delete(ctx, versionKey(accountID, ltID, n)); err != nil {
		slog.ErrorContext(ctx, "CreateLaunchTemplateVersion: failed to remove an uncommitted version", "launchTemplateId", ltID, "version", n, "err", err)
	}
}

// templateNotFound is the NotFound error for the template the request named.
func templateNotFound(ltID, name *string) error {
	if id := aws.StringValue(ltID); id != "" {
		return awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateIdNotFound,
			"The specified launch template, with template ID %s, does not exist.", id)
	}
	return awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException,
		"The specified launch template, with template name %s, does not exist.", aws.StringValue(name))
}

// --- ModifyLaunchTemplate ---

// ModifyLaunchTemplate implements the EC2 action. ClientToken is accepted but not stored: this only
// moves the default version under a CAS, so applying the same request twice converges on the same
// state and creates nothing.
func (s *LaunchTemplateServiceImpl) ModifyLaunchTemplate(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, accountID string) (*ec2.ModifyLaunchTemplateOutput, error) {
	header, entry, err := s.resolveHeader(ctx, accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}

	if sel := aws.StringValue(input.DefaultVersion); sel != "" {
		notFound := fmt.Sprintf("The launch template version %s is not found for the specified launch template.", sel)
		for attempt := 1; ; attempt++ {
			n, err := s.resolveVersionNumber(ctx, accountID, header, sel)
			if err != nil {
				return nil, withVersionNotFoundMessage(err, notFound)
			}
			// Verify the target body exists immediately before the header CAS. A
			// version delete commits through the header too, so a delete that
			// lands in between fails this CAS and the retry finds no body.
			if _, err := s.getVersion(ctx, accountID, header.LaunchTemplateId, n); err != nil {
				return nil, withVersionNotFoundMessage(err, notFound)
			}
			header.DefaultVersionNumber = n
			data, err := json.Marshal(header)
			if err != nil {
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			_, err = s.kv.Update(ctx, kvutil.AccountKey(accountID, header.LaunchTemplateId), data, entry.Revision())
			if err == nil {
				break
			}
			if !isRevisionConflict(err) || attempt >= maxVersionRetries {
				slog.ErrorContext(ctx, "ModifyLaunchTemplate: header update failed", "launchTemplateId", header.LaunchTemplateId, "attempt", attempt, "err", err)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			header, entry, err = s.getHeaderByID(ctx, accountID, header.LaunchTemplateId)
			if err != nil {
				if err.Error() == awserrors.ErrorInvalidLaunchTemplateIdNotFound {
					return nil, templateNotFound(input.LaunchTemplateId, input.LaunchTemplateName)
				}
				return nil, err
			}
		}
	}

	latest, err := s.latestVersionNumber(ctx, accountID, header.LaunchTemplateId)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "ModifyLaunchTemplate completed", "launchTemplateId", header.LaunchTemplateId, "defaultVersion", header.DefaultVersionNumber, "accountID", accountID)
	return &ec2.ModifyLaunchTemplateOutput{LaunchTemplate: headerToEC2(header, latest)}, nil
}

// --- DeleteLaunchTemplate ---

// DeleteLaunchTemplate deletes a template and every version. A store failure
// returns an error with the header still in place, so the template stays
// visible and a retried delete finishes it.
func (s *LaunchTemplateServiceImpl) DeleteLaunchTemplate(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, accountID string) (*ec2.DeleteLaunchTemplateOutput, error) {
	header, entry, err := s.resolveHeader(ctx, accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}

	latest, err := s.latestVersionNumber(ctx, accountID, header.LaunchTemplateId)
	if err != nil {
		return nil, err
	}
	out := headerToEC2(header, latest)

	if err := s.deleteTemplate(ctx, accountID, header, entry); err != nil {
		slog.ErrorContext(ctx, "DeleteLaunchTemplate failed", "launchTemplateId", header.LaunchTemplateId, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeleteLaunchTemplate completed", "launchTemplateId", header.LaunchTemplateId, "accountID", accountID)
	return &ec2.DeleteLaunchTemplateOutput{LaunchTemplate: out}, nil
}

// maxHeaderDeleteAttempts bounds restarts when the header changes mid-delete.
const maxHeaderDeleteAttempts = 3

// deleteTemplate removes every version body, then the name index, then the
// header at the revision read, and last any body a concurrent version create
// added meanwhile. The header goes last so every earlier failure is retryable
// through the public delete; a header changed in between restarts the sweep.
func (s *LaunchTemplateServiceImpl) deleteTemplate(ctx context.Context, accountID string, header *LaunchTemplateHeader, entry jetstream.KeyValueEntry) error {
	ltID := header.LaunchTemplateId
	headerKey := kvutil.AccountKey(accountID, ltID)
	for attempt := 1; ; attempt++ {
		if err := s.deleteVersionBodies(ctx, accountID, ltID); err != nil {
			return err
		}
		if err := s.releaseName(ctx, accountID, header.LaunchTemplateName, ltID); err != nil {
			return err
		}
		err := s.kv.Delete(ctx, headerKey, jetstream.LastRevision(entry.Revision()))
		if err == nil {
			break
		}
		if (!errors.Is(err, jetstream.ErrKeyRevisionMismatch) && !errors.Is(err, jetstream.ErrKeyExists)) || attempt >= maxHeaderDeleteAttempts {
			return fmt.Errorf("delete header: %w", err)
		}
		next, gerr := s.kv.Get(ctx, headerKey)
		if errors.Is(gerr, jetstream.ErrKeyNotFound) {
			break
		}
		if gerr != nil {
			return fmt.Errorf("re-read header: %w", gerr)
		}
		entry = next
	}
	s.clearRecordTags(ctx, accountID, ltID)
	// A version create in flight fails its header commit and removes its own
	// body; this sweep catches one that committed nothing before stopping.
	return s.deleteVersionBodies(ctx, accountID, ltID)
}

// deleteVersionBodies deletes every version body of ltID, stopping at the
// first failure so the caller can keep the header for a retry.
func (s *LaunchTemplateServiceImpl) deleteVersionBodies(ctx context.Context, accountID, ltID string) error {
	nums, err := s.listVersionNumbers(ctx, accountID, ltID)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	for _, n := range nums {
		if err := s.kv.Delete(ctx, versionKey(accountID, ltID, n)); err != nil {
			return fmt.Errorf("delete version %d: %w", n, err)
		}
	}
	return nil
}

// CentralTagStore clears a deleted template's entry from the central EC2 tag
// index. Implemented by domains/ec2/tags.TagsServiceImpl.
type CentralTagStore interface {
	DeleteAllTags(ctx context.Context, accountID, resourceID string) error
}

// SetCentralTagStore injects the central tag index that delete clears.
func (s *LaunchTemplateServiceImpl) SetCentralTagStore(st CentralTagStore) {
	s.centralTags = st
}

// clearRecordTags drops a deleted template's tag-index entry. It runs only
// after the header delete committed; a failure is logged, as a retried delete
// would find nothing, and the entry is left as tag residue.
func (s *LaunchTemplateServiceImpl) clearRecordTags(ctx context.Context, accountID, ltID string) {
	if s.centralTags == nil {
		return
	}
	if err := s.centralTags.DeleteAllTags(ctx, accountID, ltID); err != nil {
		slog.ErrorContext(ctx, "central tag store clear failed", "launchTemplateId", ltID, "err", err)
	}
}

// --- DeleteLaunchTemplateVersions ---

func (s *LaunchTemplateServiceImpl) DeleteLaunchTemplateVersions(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, accountID string) (*ec2.DeleteLaunchTemplateVersionsOutput, error) {
	header, entry, err := s.resolveHeader(ctx, accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}
	if len(input.Versions) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Naming the default version fails the whole request, so nothing is deleted.
	for _, v := range input.Versions {
		if n, perr := strconv.ParseInt(aws.StringValue(v), 10, 64); perr == nil && n == header.DefaultVersionNumber {
			return nil, errDefaultVersionDelete()
		}
	}

	out := &ec2.DeleteLaunchTemplateVersionsOutput{}
	deleted := map[int64][]byte{}

	for _, v := range input.Versions {
		sel := aws.StringValue(v)
		n, perr := strconv.ParseInt(sel, 10, 64)
		if perr != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, 0, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, "invalid version number"))
			continue
		}
		body, err := s.kv.Get(ctx, versionKey(accountID, header.LaunchTemplateId, n))
		if err != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, n, launchTemplateVersionDoesNotExist, "The launch template version does not exist."))
			continue
		}
		if err := s.kv.Delete(ctx, versionKey(accountID, header.LaunchTemplateId, n)); err != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, n, awserrors.ErrorServerInternal, "failed to delete version"))
			continue
		}
		deleted[n] = body.Value()
		out.SuccessfullyDeletedLaunchTemplateVersions = append(out.SuccessfullyDeletedLaunchTemplateVersions,
			&ec2.DeleteLaunchTemplateVersionsResponseSuccessItem{
				LaunchTemplateId:   aws.String(header.LaunchTemplateId),
				LaunchTemplateName: aws.String(header.LaunchTemplateName),
				VersionNumber:      aws.Int64(n),
			})
	}

	if len(deleted) > 0 {
		if err := s.commitVersionDeletes(ctx, accountID, header.LaunchTemplateId, entry, deleted); err != nil {
			switch {
			case errors.Is(err, errTemplateGone):
				return nil, templateNotFound(input.LaunchTemplateId, input.LaunchTemplateName)
			case errors.Is(err, errDefaultDeleted):
				return nil, errDefaultVersionDelete()
			}
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.InfoContext(ctx, "DeleteLaunchTemplateVersions completed", "launchTemplateId", header.LaunchTemplateId, "deleted", len(out.SuccessfullyDeletedLaunchTemplateVersions), "failed", len(out.UnsuccessfullyDeletedLaunchTemplateVersions), "accountID", accountID)
	return out, nil
}

// errDefaultDeleted: a concurrent change made a deleted version the default.
var errDefaultDeleted = errors.New("deleted version became the default")

func errDefaultVersionDelete() error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The default version cannot be deleted. Either specify another version as default or delete the launch template.")
}

// commitVersionDeletes commits deleted bodies through the header revision, so
// a default change that landed meanwhile is seen. If it names a deleted version,
// or the commit fails, the bodies are restored; a deleted template keeps none.
func (s *LaunchTemplateServiceImpl) commitVersionDeletes(ctx context.Context, accountID, ltID string, entry jetstream.KeyValueEntry, deleted map[int64][]byte) error {
	err := s.commitHeader(ctx, "DeleteLaunchTemplateVersions", accountID, ltID, entry, func(h *LaunchTemplateHeader) error {
		if _, ok := deleted[h.DefaultVersionNumber]; ok {
			return errDefaultDeleted
		}
		return nil
	})
	if err == nil || errors.Is(err, errTemplateGone) {
		return err
	}
	for n, body := range deleted {
		if _, cerr := s.kv.Create(ctx, versionKey(accountID, ltID, n), body); cerr != nil {
			slog.ErrorContext(ctx, "DeleteLaunchTemplateVersions: failed to restore a version", "launchTemplateId", ltID, "version", n, "err", cerr)
		}
	}
	return err
}

// launchTemplateVersionDoesNotExist is the per-version code AWS reports in a
// DeleteLaunchTemplateVersions result; it is never a request-level error.
const launchTemplateVersionDoesNotExist = "launchTemplateVersionDoesNotExist"

func deleteErrorItem(h *LaunchTemplateHeader, n int64, code, msg string) *ec2.DeleteLaunchTemplateVersionsResponseErrorItem {
	item := &ec2.DeleteLaunchTemplateVersionsResponseErrorItem{
		LaunchTemplateId:   aws.String(h.LaunchTemplateId),
		LaunchTemplateName: aws.String(h.LaunchTemplateName),
		ResponseError:      &ec2.ResponseError{Code: aws.String(code), Message: aws.String(msg)},
	}
	if n > 0 {
		item.VersionNumber = aws.Int64(n)
	}
	return item
}

// --- DescribeLaunchTemplates ---

var describeLaunchTemplatesValidFilters = map[string]bool{
	"create-time":          true,
	"launch-template-name": true,
	"launch-template-id":   true,
	"tag-key":              true,
}

func (s *LaunchTemplateServiceImpl) DescribeLaunchTemplates(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, accountID string) (*ec2.DescribeLaunchTemplatesOutput, error) {
	filters, err := awsfilters.ParseFilters(input.Filters, describeLaunchTemplatesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeLaunchTemplates: invalid filter", "err", err)
		return nil, err
	}

	for _, id := range input.LaunchTemplateIds {
		if err := validateTemplateID(aws.StringValue(id)); err != nil {
			return nil, err
		}
	}

	nameSet := stringSet(input.LaunchTemplateNames)
	idSet := stringSet(input.LaunchTemplateIds)

	prefix := accountID + "."
	keys, err := s.kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Derive every template's latest version number from this single key list so
	// the loop below does not re-scan the whole bucket once per matched template.
	latestByID := latestVersionsFromKeys(keys, prefix)

	var templates []*ec2.LaunchTemplate
	foundNames := make(map[string]bool)
	foundIDs := make(map[string]bool)
	for _, k := range keys {
		if !isHeaderKey(k, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, k)
		if err != nil {
			slog.WarnContext(ctx, "DescribeLaunchTemplates: header read failed", "key", k, "err", err)
			continue
		}
		var h LaunchTemplateHeader
		if err := json.Unmarshal(entry.Value(), &h); err != nil {
			slog.WarnContext(ctx, "DescribeLaunchTemplates: bad header record", "key", k, "err", err)
			continue
		}
		if len(nameSet) > 0 && !nameSet[h.LaunchTemplateName] {
			continue
		}
		if len(idSet) > 0 && !idSet[h.LaunchTemplateId] {
			continue
		}
		if !templateMatchesFilters(&h, filters) {
			continue
		}
		foundNames[h.LaunchTemplateName] = true
		foundIDs[h.LaunchTemplateId] = true
		templates = append(templates, headerToEC2(&h, latestByID[h.LaunchTemplateId]))
	}

	const missing = "At least one of the launch templates specified in the request does not exist."
	for name := range nameSet {
		if !foundNames[name] {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException, missing)
		}
	}
	for id := range idSet {
		if !foundIDs[id] {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateIdNotFound, missing)
		}
	}

	slog.InfoContext(ctx, "DescribeLaunchTemplates completed", "count", len(templates), "accountID", accountID)
	return &ec2.DescribeLaunchTemplatesOutput{LaunchTemplates: templates}, nil
}

func templateMatchesFilters(h *LaunchTemplateHeader, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "launch-template-id":
			if !awsfilters.MatchesAny(values, h.LaunchTemplateId) {
				return false
			}
		case "launch-template-name":
			if !awsfilters.MatchesAny(values, h.LaunchTemplateName) {
				return false
			}
		case "create-time":
			if !awsfilters.MatchesAny(values, h.CreateTime.Format(time.RFC3339)) {
				return false
			}
		case "tag-key":
			matched := false
			for k := range h.Tags {
				if awsfilters.MatchesAny(values, k) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		default:
			return false
		}
	}
	return awsfilters.MatchesTags(filters, h.Tags)
}

// --- DescribeLaunchTemplateVersions ---

var describeLaunchTemplateVersionsValidFilters = map[string]bool{
	"is-default-version": true,
	"image-id":           true,
	"instance-type":      true,
	"kernel-id":          true,
	"ram-disk-id":        true,
	"ebs-optimized":      true,
}

func (s *LaunchTemplateServiceImpl) DescribeLaunchTemplateVersions(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, accountID string) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	filters, err := awsfilters.ParseFilters(input.Filters, describeLaunchTemplateVersionsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeLaunchTemplateVersions: invalid filter", "err", err)
		return nil, err
	}

	header, _, err := s.resolveHeader(ctx, accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}

	numbers, err := s.selectVersionNumbers(ctx, accountID, header, input)
	if err != nil {
		return nil, err
	}

	var versions []*ec2.LaunchTemplateVersion
	for _, n := range numbers {
		rec, err := s.getVersion(ctx, accountID, header.LaunchTemplateId, n)
		if err != nil {
			return nil, err
		}
		if !versionMatchesFilters(rec, n == header.DefaultVersionNumber, filters) {
			continue
		}
		versions = append(versions, versionRecToEC2(header, rec, n == header.DefaultVersionNumber))
	}

	slog.InfoContext(ctx, "DescribeLaunchTemplateVersions completed", "launchTemplateId", header.LaunchTemplateId, "count", len(versions), "accountID", accountID)
	return &ec2.DescribeLaunchTemplateVersionsOutput{LaunchTemplateVersions: versions}, nil
}

// selectVersionNumbers resolves the requested Versions/MinVersion/MaxVersion into
// a concrete, ordered list of version numbers. Explicit Versions each resolve and
// must exist; otherwise all versions within the optional [Min,Max] range.
func (s *LaunchTemplateServiceImpl) selectVersionNumbers(ctx context.Context, accountID string, header *LaunchTemplateHeader, input *ec2.DescribeLaunchTemplateVersionsInput) ([]int64, error) {
	if len(input.Versions) > 0 {
		seen := make(map[int64]bool)
		var out []int64
		for _, v := range input.Versions {
			notFound := fmt.Sprintf("Could not find the specified version %s for the launch template with ID %s.",
				aws.StringValue(v), header.LaunchTemplateId)
			n, err := s.resolveVersionNumber(ctx, accountID, header, aws.StringValue(v))
			if err != nil {
				return nil, withVersionNotFoundMessage(err, notFound)
			}
			if _, err := s.getVersion(ctx, accountID, header.LaunchTemplateId, n); err != nil {
				return nil, withVersionNotFoundMessage(err, notFound)
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
		return out, nil
	}

	all, err := s.listVersionNumbers(ctx, accountID, header.LaunchTemplateId)
	if err != nil {
		return nil, err
	}
	minV, err := parseBoundVersion(aws.StringValue(input.MinVersion))
	if err != nil {
		return nil, err
	}
	maxV, err := parseBoundVersion(aws.StringValue(input.MaxVersion))
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, n := range all {
		if minV > 0 && n < minV {
			continue
		}
		if maxV > 0 && n > maxV {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func parseBoundVersion(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return n, nil
}

func versionMatchesFilters(rec *LaunchTemplateVersionRec, isDefault bool, filters map[string][]string) bool {
	d := rec.Data
	for name, values := range filters {
		switch name {
		case "is-default-version":
			if !awsfilters.MatchesAny(values, strconv.FormatBool(isDefault)) {
				return false
			}
		case "image-id":
			if d == nil || !awsfilters.MatchesAny(values, aws.StringValue(d.ImageId)) {
				return false
			}
		case "instance-type":
			if d == nil || !awsfilters.MatchesAny(values, aws.StringValue(d.InstanceType)) {
				return false
			}
		case "kernel-id":
			if d == nil || !awsfilters.MatchesAny(values, aws.StringValue(d.KernelId)) {
				return false
			}
		case "ram-disk-id":
			if d == nil || !awsfilters.MatchesAny(values, aws.StringValue(d.RamDiskId)) {
				return false
			}
		case "ebs-optimized":
			if d == nil || !awsfilters.MatchesAny(values, strconv.FormatBool(aws.BoolValue(d.EbsOptimized))) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// --- converters ---

// headerToEC2 builds the AWS LaunchTemplate view; latest is the highest existing
// version number (0 when the template has no versions).
func headerToEC2(h *LaunchTemplateHeader, latest int64) *ec2.LaunchTemplate {
	lt := &ec2.LaunchTemplate{
		LaunchTemplateId:     aws.String(h.LaunchTemplateId),
		LaunchTemplateName:   aws.String(h.LaunchTemplateName),
		CreatedBy:            aws.String(h.CreatedBy),
		CreateTime:           aws.Time(h.CreateTime),
		DefaultVersionNumber: aws.Int64(h.DefaultVersionNumber),
		Tags:                 awstags.MapToEC2(h.Tags),
	}
	if latest > 0 {
		lt.LatestVersionNumber = aws.Int64(latest)
	}
	return lt
}

// versionToEC2 builds the AWS view for a freshly-created version (its number is
// on the record).
func versionToEC2(h *LaunchTemplateHeader, rec *LaunchTemplateVersionRec) *ec2.LaunchTemplateVersion {
	return versionRecToEC2(h, rec, rec.VersionNumber == h.DefaultVersionNumber)
}

func versionRecToEC2(h *LaunchTemplateHeader, rec *LaunchTemplateVersionRec, isDefault bool) *ec2.LaunchTemplateVersion {
	return &ec2.LaunchTemplateVersion{
		LaunchTemplateId:   aws.String(h.LaunchTemplateId),
		LaunchTemplateName: aws.String(h.LaunchTemplateName),
		VersionNumber:      aws.Int64(rec.VersionNumber),
		VersionDescription: aws.String(rec.VersionDescription),
		CreateTime:         aws.Time(rec.CreateTime),
		CreatedBy:          aws.String(rec.CreatedBy),
		DefaultVersion:     aws.Bool(isDefault),
		LaunchTemplateData: rec.Data,
	}
}

// --- misc helpers ---

// withVersionNotFoundMessage attaches AWS's message, which differs by action,
// to a VersionNotFound error and passes any other error through.
func withVersionNotFoundMessage(err error, message string) error {
	if err.Error() != awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound {
		return err
	}
	return awserrors.Errorf(awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, "%s", message)
}

// isHeaderKey reports whether k is a template header key (account.lt-<id>) as
// opposed to a name index, version body (account.lt-<id>.v<n>), or _version key.
func isHeaderKey(k, prefix string) bool {
	if !strings.HasPrefix(k, prefix) {
		return false
	}
	rest := k[len(prefix):]
	return strings.HasPrefix(rest, "lt-") && !strings.Contains(rest, ".")
}

func stringSet(in []*string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	set := make(map[string]bool, len(in))
	for _, s := range in {
		if s != nil {
			set[*s] = true
		}
	}
	return set
}

// validateTemplateID enforces the lt- id prefix so a syntactically invalid id
// returns Malformed rather than NotFound, matching image/volume id handling.
func validateTemplateID(id string) error {
	if !strings.HasPrefix(id, "lt-") {
		return errors.New(awserrors.ErrorInvalidLaunchTemplateIdMalformed)
	}
	return nil
}

// validateTemplateName enforces the AWS 3-128 char and allowed-character rules.
func validateTemplateName(name string) error {
	if name == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(name) < 3 || len(name) > 128 {
		return errors.New(awserrors.ErrorInvalidLaunchTemplateNameMalformedException)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("-_./()", c):
		default:
			return errors.New(awserrors.ErrorInvalidLaunchTemplateNameMalformedException)
		}
	}
	return nil
}
