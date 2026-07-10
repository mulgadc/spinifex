package handlers_ec2_launchtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
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
	kv       nats.KeyValue
}

// NewLaunchTemplateServiceImplWithNATS creates a launch template service with NATS JetStream.
func NewLaunchTemplateServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*LaunchTemplateServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := utils.GetOrCreateKVBucket(js, KVBucketLaunchTemplates, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketLaunchTemplates, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketLaunchTemplates, kv, KVBucketLaunchTemplatesVersion); err != nil {
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

// headerKey returns the header key: account.lt-<id>.
func headerKey(accountID, ltID string) string {
	return utils.AccountKey(accountID, ltID)
}

// nameKey returns the name-index key: account.name.<name>.
func nameKey(accountID, name string) string {
	return utils.AccountKey(accountID, "name."+name)
}

// versionKey returns a version-body key: account.lt-<id>.v<n>.
func versionKey(accountID, ltID string, n int64) string {
	return utils.AccountKey(accountID, ltID+".v"+strconv.FormatInt(n, 10))
}

// versionPrefix returns the scan prefix for a template's version bodies.
func versionPrefix(accountID, ltID string) string {
	return utils.AccountKey(accountID, ltID+".v")
}

// --- record load helpers ---

// getHeaderByID reads a header and its KV entry (for CAS) by launch template id.
func (s *LaunchTemplateServiceImpl) getHeaderByID(accountID, ltID string) (*LaunchTemplateHeader, nats.KeyValueEntry, error) {
	entry, err := s.kv.Get(headerKey(accountID, ltID))
	if err != nil {
		return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateIdNotFound)
	}
	var h LaunchTemplateHeader
	if err := json.Unmarshal(entry.Value(), &h); err != nil {
		return nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &h, entry, nil
}

// getHeaderByName resolves a template name to its header via the name index.
// A dangling name index (header missing) is treated as not-found.
func (s *LaunchTemplateServiceImpl) getHeaderByName(accountID, name string) (*LaunchTemplateHeader, nats.KeyValueEntry, error) {
	entry, err := s.kv.Get(nameKey(accountID, name))
	if err != nil {
		return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException)
	}
	ltID := string(entry.Value())
	h, hentry, err := s.getHeaderByID(accountID, ltID)
	if err != nil {
		return nil, nil, errors.New(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException)
	}
	return h, hentry, nil
}

// resolveHeader resolves a template by id or name (mutually exclusive) and
// returns the header plus its KV entry for CAS operations.
func (s *LaunchTemplateServiceImpl) resolveHeader(accountID string, ltID, name *string) (*LaunchTemplateHeader, nats.KeyValueEntry, error) {
	id := aws.StringValue(ltID)
	nm := aws.StringValue(name)
	switch {
	case id != "" && nm != "":
		return nil, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	case id != "":
		if err := validateTemplateID(id); err != nil {
			return nil, nil, err
		}
		return s.getHeaderByID(accountID, id)
	case nm != "":
		return s.getHeaderByName(accountID, nm)
	default:
		return nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}
}

// getVersion loads an immutable version body. A missing body is the read-side
// guard that turns a dangling default or deleted version into VersionNotFound.
func (s *LaunchTemplateServiceImpl) getVersion(accountID, ltID string, n int64) (*LaunchTemplateVersionRec, error) {
	entry, err := s.kv.Get(versionKey(accountID, ltID, n))
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
func (s *LaunchTemplateServiceImpl) listVersionNumbers(accountID, ltID string) ([]int64, error) {
	keys, err := s.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
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
	for i := 1; i < len(nums); i++ {
		for j := i; j > 0 && nums[j-1] > nums[j]; j-- {
			nums[j-1], nums[j] = nums[j], nums[j-1]
		}
	}
	return nums, nil
}

// latestVersionNumber returns the highest existing version number, or 0 if none.
func (s *LaunchTemplateServiceImpl) latestVersionNumber(accountID, ltID string) (int64, error) {
	nums, err := s.listVersionNumbers(accountID, ltID)
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
		sep := strings.Index(rest, ".v")
		if sep < 0 {
			continue
		}
		n, err := strconv.ParseInt(rest[sep+2:], 10, 64)
		if err != nil {
			continue
		}
		if id := rest[:sep]; n > latest[id] {
			latest[id] = n
		}
	}
	return latest
}

// resolveVersionNumber maps a selector ("", $Default, $Latest, or numeric) to a
// concrete version number. It does not verify the body exists — callers load the
// body and surface VersionNotFound when it is missing.
func (s *LaunchTemplateServiceImpl) resolveVersionNumber(accountID string, h *LaunchTemplateHeader, sel string) (int64, error) {
	switch sel {
	case "", versionDefault:
		return h.DefaultVersionNumber, nil
	case versionLatest:
		return s.latestVersionNumber(accountID, h.LaunchTemplateId)
	default:
		n, err := strconv.ParseInt(sel, 10, 64)
		if err != nil {
			return 0, errors.New(awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound)
		}
		return n, nil
	}
}

// --- CreateLaunchTemplate ---

func (s *LaunchTemplateServiceImpl) CreateLaunchTemplate(ctx context.Context, input *ec2.CreateLaunchTemplateInput, accountID string) (*ec2.CreateLaunchTemplateOutput, error) {
	if input.LaunchTemplateData == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	name := aws.StringValue(input.LaunchTemplateName)
	if err := validateTemplateName(name); err != nil {
		return nil, err
	}
	if aws.BoolValue(input.DryRun) {
		return &ec2.CreateLaunchTemplateOutput{}, nil
	}

	data, err := requestToResponse(input.LaunchTemplateData)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	ltID := utils.GenerateResourceID("lt")
	if err := s.claimName(accountID, name, ltID); err != nil {
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
	if err := s.putVersion(accountID, ltID, &rec); err != nil {
		return nil, err
	}

	header := LaunchTemplateHeader{
		LaunchTemplateId:     ltID,
		LaunchTemplateName:   name,
		AccountID:            accountID,
		CreatedBy:            accountID,
		CreateTime:           now,
		DefaultVersionNumber: 1,
		Tags:                 utils.ExtractTags(input.TagSpecifications, launchTemplateTagResourceType),
	}
	if err := s.putHeader(accountID, &header); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "CreateLaunchTemplate completed", "launchTemplateId", ltID, "name", name, "accountID", accountID)

	return &ec2.CreateLaunchTemplateOutput{
		LaunchTemplate: headerToEC2(&header, 1),
	}, nil
}

// claimName atomically reserves a template name via kv.Create, with
// repair-on-write: if the name already exists but its header is gone (a crash
// orphan), reclaim it via a revision-guarded CAS update. Only a name whose header
// still exists returns AlreadyExists.
func (s *LaunchTemplateServiceImpl) claimName(accountID, name, ltID string) error {
	key := nameKey(accountID, name)
	if _, err := s.kv.Create(key, []byte(ltID)); err == nil {
		return nil
	}
	entry, err := s.kv.Get(key)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	// Only a genuinely missing header is a crash orphan safe to reclaim. A live
	// header means the name is taken; any other read error (transient fault, or a
	// concurrent in-flight create whose header is not yet written) fails closed so
	// the name is never stolen from a possibly-live template.
	_, herr := s.kv.Get(headerKey(accountID, string(entry.Value())))
	switch {
	case herr == nil:
		return errors.New(awserrors.ErrorInvalidLaunchTemplateNameAlreadyExistsException)
	case !errors.Is(herr, nats.ErrKeyNotFound):
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Update(key, []byte(ltID), entry.Revision()); err != nil {
		return errors.New(awserrors.ErrorInvalidLaunchTemplateNameAlreadyExistsException)
	}
	return nil
}

func (s *LaunchTemplateServiceImpl) putHeader(accountID string, h *LaunchTemplateHeader) error {
	data, err := json.Marshal(h)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Put(headerKey(accountID, h.LaunchTemplateId), data); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

func (s *LaunchTemplateServiceImpl) putVersion(accountID, ltID string, rec *LaunchTemplateVersionRec) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Put(versionKey(accountID, ltID, rec.VersionNumber), data); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// --- CreateLaunchTemplateVersion ---

func (s *LaunchTemplateServiceImpl) CreateLaunchTemplateVersion(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, accountID string) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	if input.LaunchTemplateData == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	header, _, err := s.resolveHeader(accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}
	if aws.BoolValue(input.DryRun) {
		return &ec2.CreateLaunchTemplateVersionOutput{}, nil
	}

	override, err := requestToResponse(input.LaunchTemplateData)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// SourceVersion clones-and-overrides; without it the new version is the data as given.
	data := override
	if src := aws.StringValue(input.SourceVersion); src != "" {
		n, err := s.resolveVersionNumber(accountID, header, src)
		if err != nil {
			return nil, err
		}
		base, err := s.getVersion(accountID, header.LaunchTemplateId, n)
		if err != nil {
			return nil, err
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
		latest, err := s.latestVersionNumber(accountID, header.LaunchTemplateId)
		if err != nil {
			return nil, err
		}
		next := latest + 1
		rec.VersionNumber = next
		body, err := json.Marshal(&rec)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if _, err := s.kv.Create(versionKey(accountID, header.LaunchTemplateId, next), body); err != nil {
			if errors.Is(err, nats.ErrKeyExists) {
				slog.DebugContext(ctx, "CreateLaunchTemplateVersion: version-number collision, retrying", "attempt", attempt, "next", next)
				continue
			}
			slog.ErrorContext(ctx, "CreateLaunchTemplateVersion: version-key create failed", "next", next, "err", err)
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

// --- ModifyLaunchTemplate ---

func (s *LaunchTemplateServiceImpl) ModifyLaunchTemplate(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, accountID string) (*ec2.ModifyLaunchTemplateOutput, error) {
	header, entry, err := s.resolveHeader(accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}
	if aws.BoolValue(input.DryRun) {
		latest, err := s.latestVersionNumber(accountID, header.LaunchTemplateId)
		if err != nil {
			return nil, err
		}
		return &ec2.ModifyLaunchTemplateOutput{LaunchTemplate: headerToEC2(header, latest)}, nil
	}

	if sel := aws.StringValue(input.DefaultVersion); sel != "" {
		n, err := s.resolveVersionNumber(accountID, header, sel)
		if err != nil {
			return nil, err
		}
		// Verify the target body exists immediately before the header CAS so a
		// concurrent delete degrades to VersionNotFound, never a dangling default.
		if _, err := s.getVersion(accountID, header.LaunchTemplateId, n); err != nil {
			return nil, err
		}
		header.DefaultVersionNumber = n
		data, err := json.Marshal(header)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if _, err := s.kv.Update(headerKey(accountID, header.LaunchTemplateId), data, entry.Revision()); err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	latest, err := s.latestVersionNumber(accountID, header.LaunchTemplateId)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "ModifyLaunchTemplate completed", "launchTemplateId", header.LaunchTemplateId, "defaultVersion", header.DefaultVersionNumber, "accountID", accountID)
	return &ec2.ModifyLaunchTemplateOutput{LaunchTemplate: headerToEC2(header, latest)}, nil
}

// --- DeleteLaunchTemplate ---

func (s *LaunchTemplateServiceImpl) DeleteLaunchTemplate(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, accountID string) (*ec2.DeleteLaunchTemplateOutput, error) {
	header, _, err := s.resolveHeader(accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}
	if aws.BoolValue(input.DryRun) {
		latest, err := s.latestVersionNumber(accountID, header.LaunchTemplateId)
		if err != nil {
			return nil, err
		}
		return &ec2.DeleteLaunchTemplateOutput{LaunchTemplate: headerToEC2(header, latest)}, nil
	}

	latest, err := s.latestVersionNumber(accountID, header.LaunchTemplateId)
	if err != nil {
		return nil, err
	}
	out := headerToEC2(header, latest)

	// Delete the header first: the template immediately vanishes from every
	// describe. Version bodies and the name index are best-effort cleanup.
	if err := s.kv.Delete(headerKey(accountID, header.LaunchTemplateId)); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.kv.Delete(nameKey(accountID, header.LaunchTemplateName)); err != nil {
		slog.WarnContext(ctx, "DeleteLaunchTemplate: name index cleanup failed", "name", header.LaunchTemplateName, "err", err)
	}
	nums, _ := s.listVersionNumbers(accountID, header.LaunchTemplateId)
	for _, n := range nums {
		if err := s.kv.Delete(versionKey(accountID, header.LaunchTemplateId, n)); err != nil {
			slog.WarnContext(ctx, "DeleteLaunchTemplate: version cleanup failed", "launchTemplateId", header.LaunchTemplateId, "version", n, "err", err)
		}
	}

	slog.InfoContext(ctx, "DeleteLaunchTemplate completed", "launchTemplateId", header.LaunchTemplateId, "accountID", accountID)
	return &ec2.DeleteLaunchTemplateOutput{LaunchTemplate: out}, nil
}

// --- DeleteLaunchTemplateVersions ---

func (s *LaunchTemplateServiceImpl) DeleteLaunchTemplateVersions(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, accountID string) (*ec2.DeleteLaunchTemplateVersionsOutput, error) {
	header, _, err := s.resolveHeader(accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}
	if len(input.Versions) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	out := &ec2.DeleteLaunchTemplateVersionsOutput{}
	if aws.BoolValue(input.DryRun) {
		return out, nil
	}

	for _, v := range input.Versions {
		sel := aws.StringValue(v)
		n, perr := strconv.ParseInt(sel, 10, 64)
		if perr != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, 0, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, "invalid version number"))
			continue
		}
		if n == header.DefaultVersionNumber {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, n, awserrors.ErrorInvalidParameterValue, "cannot delete the default version of a launch template"))
			continue
		}
		if _, err := s.getVersion(accountID, header.LaunchTemplateId, n); err != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, n, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, "version does not exist"))
			continue
		}
		if err := s.kv.Delete(versionKey(accountID, header.LaunchTemplateId, n)); err != nil {
			out.UnsuccessfullyDeletedLaunchTemplateVersions = append(out.UnsuccessfullyDeletedLaunchTemplateVersions,
				deleteErrorItem(header, n, awserrors.ErrorServerInternal, "failed to delete version"))
			continue
		}
		out.SuccessfullyDeletedLaunchTemplateVersions = append(out.SuccessfullyDeletedLaunchTemplateVersions,
			&ec2.DeleteLaunchTemplateVersionsResponseSuccessItem{
				LaunchTemplateId:   aws.String(header.LaunchTemplateId),
				LaunchTemplateName: aws.String(header.LaunchTemplateName),
				VersionNumber:      aws.Int64(n),
			})
	}

	slog.InfoContext(ctx, "DeleteLaunchTemplateVersions completed", "launchTemplateId", header.LaunchTemplateId, "deleted", len(out.SuccessfullyDeletedLaunchTemplateVersions), "failed", len(out.UnsuccessfullyDeletedLaunchTemplateVersions), "accountID", accountID)
	return out, nil
}

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
	filters, err := filterutil.ParseFilters(input.Filters, describeLaunchTemplatesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeLaunchTemplates: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	for _, id := range input.LaunchTemplateIds {
		if err := validateTemplateID(aws.StringValue(id)); err != nil {
			return nil, err
		}
	}

	nameSet := stringSet(input.LaunchTemplateNames)
	idSet := stringSet(input.LaunchTemplateIds)

	prefix := accountID + "."
	keys, err := s.kv.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
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
		entry, err := s.kv.Get(k)
		if err != nil {
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

	for name := range nameSet {
		if !foundNames[name] {
			return nil, errors.New(awserrors.ErrorInvalidLaunchTemplateNameNotFoundException)
		}
	}
	for id := range idSet {
		if !foundIDs[id] {
			return nil, errors.New(awserrors.ErrorInvalidLaunchTemplateIdNotFound)
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
			if !filterutil.MatchesAny(values, h.LaunchTemplateId) {
				return false
			}
		case "launch-template-name":
			if !filterutil.MatchesAny(values, h.LaunchTemplateName) {
				return false
			}
		case "create-time":
			if !filterutil.MatchesAny(values, h.CreateTime.Format(time.RFC3339)) {
				return false
			}
		case "tag-key":
			matched := false
			for k := range h.Tags {
				if filterutil.MatchesAny(values, k) {
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
	return filterutil.MatchesTags(filters, h.Tags)
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
	filters, err := filterutil.ParseFilters(input.Filters, describeLaunchTemplateVersionsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeLaunchTemplateVersions: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	header, _, err := s.resolveHeader(accountID, input.LaunchTemplateId, input.LaunchTemplateName)
	if err != nil {
		return nil, err
	}

	numbers, err := s.selectVersionNumbers(accountID, header, input)
	if err != nil {
		return nil, err
	}

	var versions []*ec2.LaunchTemplateVersion
	for _, n := range numbers {
		rec, err := s.getVersion(accountID, header.LaunchTemplateId, n)
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
func (s *LaunchTemplateServiceImpl) selectVersionNumbers(accountID string, header *LaunchTemplateHeader, input *ec2.DescribeLaunchTemplateVersionsInput) ([]int64, error) {
	if len(input.Versions) > 0 {
		seen := make(map[int64]bool)
		var out []int64
		for _, v := range input.Versions {
			n, err := s.resolveVersionNumber(accountID, header, aws.StringValue(v))
			if err != nil {
				return nil, err
			}
			if _, err := s.getVersion(accountID, header.LaunchTemplateId, n); err != nil {
				return nil, err
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
		return out, nil
	}

	all, err := s.listVersionNumbers(accountID, header.LaunchTemplateId)
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
			if !filterutil.MatchesAny(values, strconv.FormatBool(isDefault)) {
				return false
			}
		case "image-id":
			if d == nil || !filterutil.MatchesAny(values, aws.StringValue(d.ImageId)) {
				return false
			}
		case "instance-type":
			if d == nil || !filterutil.MatchesAny(values, aws.StringValue(d.InstanceType)) {
				return false
			}
		case "kernel-id":
			if d == nil || !filterutil.MatchesAny(values, aws.StringValue(d.KernelId)) {
				return false
			}
		case "ram-disk-id":
			if d == nil || !filterutil.MatchesAny(values, aws.StringValue(d.RamDiskId)) {
				return false
			}
		case "ebs-optimized":
			if d == nil || !filterutil.MatchesAny(values, strconv.FormatBool(aws.BoolValue(d.EbsOptimized))) {
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
		Tags:                 utils.MapToEC2Tags(h.Tags),
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
