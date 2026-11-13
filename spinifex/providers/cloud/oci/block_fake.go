package oci

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxISCSIAttachmentsPerInstance is OCI's cap on iSCSI attachments to one
// instance, the same on every VM and bare-metal shape.
const MaxISCSIAttachmentsPerInstance = 32

// ErrReplyLost is what BlockFake returns for a call LoseReply staged: the
// call took effect in OCI but its answer never arrived.
var ErrReplyLost = errors.New("oci fake: reply lost after the call took effect")

// BlockFake is an in-memory BlockClient. It models the OCI behaviours an EBS
// provider has to handle: the 50 GB floor, retry tokens, backups that stay
// CREATING until a test settles them, and attachments that move by state.
type BlockFake struct {
	stagedFailures

	mu sync.Mutex

	// AttachAfter and DetachAfter are how many GetVolumeAttachment calls an
	// attachment spends ATTACHING or DETACHING before it settles. Zero means
	// it settles in the call that started it.
	AttachAfter int
	DetachAfter int

	// AttachmentCap caps iSCSI attachments per instance; zero means
	// MaxISCSIAttachmentsPerInstance.
	AttachmentCap int

	volumes     map[string]*BlockVolume
	backups     map[string]*VolumeBackup
	attachments map[string]*fakeAttachment
	tokens      map[string]string // op and retry token -> resource OCID
	tokenOf     map[string]string // resource OCID -> the retry token it was created with
	loseReply   map[string]bool
	seq         int
}

type fakeAttachment struct {
	VolumeAttachment

	countdown int
}

var _ BlockClient = (*BlockFake)(nil)

// NewBlockFake returns an empty BlockFake whose attachments spend one read in
// each transitional state, so a caller that does not wait is caught.
func NewBlockFake() *BlockFake {
	return &BlockFake{
		AttachAfter: 1,
		DetachAfter: 1,
		volumes:     map[string]*BlockVolume{},
		backups:     map[string]*VolumeBackup{},
		attachments: map[string]*fakeAttachment{},
		tokens:      map[string]string{},
		tokenOf:     map[string]string{},
		loseReply:   map[string]bool{},

		stagedFailures: stagedFailures{FailWith: map[string]error{}},
	}
}

// LoseReply makes the next call to op take effect and return ErrReplyLost.
func (f *BlockFake) LoseReply(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseReply[op] = true
}

// SettleBackup moves a backup still in progress to state, as OCI does once
// the backup finishes or fails.
func (f *BlockFake) SettleBackup(backupID string, state BackupState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.backups[backupID]
	if !ok {
		return fmt.Errorf("oci fake: no backup %s", backupID)
	}
	if !b.State.InProgress() {
		return fmt.Errorf("oci fake: backup %s is %s, not in progress", backupID, b.State)
	}
	b.State = state
	return nil
}

// SeedAttachment inserts an attachment directly, for state OCI holds that no
// call in the test made, such as another instance's attachment.
func (f *BlockFake) SeedAttachment(a VolumeAttachment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachments[a.ID] = &fakeAttachment{VolumeAttachment: a}
}

// Volumes returns every volume the fake holds, terminated ones included.
func (f *BlockFake) Volumes() []BlockVolume {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BlockVolume, 0, len(f.volumes))
	for _, v := range f.volumes {
		out = append(out, cloneVolume(*v))
	}
	return out
}

// Backups returns every backup the fake holds, terminated ones included.
func (f *BlockFake) Backups() []VolumeBackup {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]VolumeBackup, 0, len(f.backups))
	for _, b := range f.backups {
		out = append(out, cloneBackup(*b))
	}
	return out
}

// RetryToken is the opc-retry-token the resource was created with.
func (f *BlockFake) RetryToken(resourceID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenOf[resourceID]
}

func (f *BlockFake) CreateVolume(_ context.Context, in CreateVolumeInput) (BlockVolume, error) {
	const op = "CreateVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return BlockVolume{}, err
	}
	if id, ok := f.replayed(op, in.RetryToken); ok {
		return answerOf(f, op, cloneVolume(*f.volumes[id]))
	}
	if in.CompartmentID == "" || in.AvailabilityDomain == "" {
		return BlockVolume{}, fakeError(op, 400, "InvalidParameter", "compartmentId and availabilityDomain are required")
	}
	if in.SizeGB < MinVolumeSizeGB {
		return BlockVolume{}, fakeError(op, 400, "InvalidParameter", fmt.Sprintf("sizeInGBs must be at least %d", MinVolumeSizeGB))
	}
	if in.SourceBackupID != "" {
		b, ok := f.backups[in.SourceBackupID]
		if !ok || b.State.Gone() {
			return BlockVolume{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume backup not found")
		}
		if b.State != BackupAvailable {
			return BlockVolume{}, fakeError(op, 409, "Conflict", "volume backup is not available")
		}
		if in.SizeGB < b.SizeGB {
			return BlockVolume{}, fakeError(op, 400, "InvalidParameter", "sizeInGBs is smaller than the backup")
		}
	}
	vpus := in.VPUsPerGB
	if vpus == 0 {
		vpus = 10
	}
	id := f.newID("volume")
	f.volumes[id] = &BlockVolume{
		ID: id, CompartmentID: in.CompartmentID, AvailabilityDomain: in.AvailabilityDomain,
		DisplayName: in.DisplayName, SizeGB: in.SizeGB, VPUsPerGB: vpus, State: VolumeAvailable,
		SourceBackupID: in.SourceBackupID, FreeformTags: maps.Clone(in.FreeformTags), TimeCreated: time.Now().UTC(),
		ETag: "1",
	}
	f.remember(op, in.RetryToken, id)
	return answerOf(f, op, cloneVolume(*f.volumes[id]))
}

func (f *BlockFake) GetVolume(_ context.Context, volumeID string) (BlockVolume, error) {
	const op = "GetVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return BlockVolume{}, err
	}
	v, ok := f.volumes[volumeID]
	if !ok {
		return BlockVolume{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume not found")
	}
	return cloneVolume(*v), nil
}

func (f *BlockFake) ListVolumes(_ context.Context, filter VolumeFilter) ([]BlockVolume, error) {
	const op = "ListVolumes"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return nil, err
	}
	var out []BlockVolume
	for _, v := range f.volumes {
		if v.CompartmentID != filter.CompartmentID ||
			(filter.AvailabilityDomain != "" && v.AvailabilityDomain != filter.AvailabilityDomain) ||
			(filter.DisplayName != "" && v.DisplayName != filter.DisplayName) ||
			!hasTags(v.FreeformTags, filter.Tags) {
			continue
		}
		listed := cloneVolume(*v)
		listed.ETag = ""
		out = append(out, listed)
	}
	return out, nil
}

func (f *BlockFake) UpdateVolume(_ context.Context, volumeID string, in UpdateVolumeInput) (BlockVolume, error) {
	const op = "UpdateVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return BlockVolume{}, err
	}
	v, ok := f.volumes[volumeID]
	if !ok || v.State.Gone() {
		return BlockVolume{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume not found")
	}
	if in.IfMatch != "" && in.IfMatch != v.ETag {
		return BlockVolume{}, fakeError(op, 412, "NoEtagMatch", "the volume changed since the given etag")
	}
	if in.SizeGB > 0 && in.SizeGB < v.SizeGB {
		return BlockVolume{}, fakeError(op, 400, "InvalidParameter", "a volume cannot shrink")
	}
	if in.SizeGB > 0 {
		v.SizeGB = in.SizeGB
	}
	if in.FreeformTags != nil {
		v.FreeformTags = maps.Clone(in.FreeformTags)
	}
	rev, _ := strconv.Atoi(v.ETag)
	v.ETag = strconv.Itoa(rev + 1)
	return answerOf(f, op, cloneVolume(*v))
}

func (f *BlockFake) DeleteVolume(_ context.Context, volumeID string) error {
	const op = "DeleteVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return err
	}
	v, ok := f.volumes[volumeID]
	if !ok || v.State.Gone() {
		return fakeError(op, 404, "NotAuthorizedOrNotFound", "volume not found")
	}
	for _, b := range f.backups {
		if b.VolumeID == volumeID && b.State.InProgress() {
			return fakeError(op, 409, "Conflict", "There is currently a backup operation in progress for Volume "+volumeID+".")
		}
	}
	for _, a := range f.attachments {
		if a.VolumeID == volumeID && a.State != AttachmentDetached {
			return fakeError(op, 409, "Conflict", "volume is attached")
		}
	}
	v.State = VolumeTerminated
	_, err := answerOf(f, op, struct{}{})
	return err
}

func (f *BlockFake) AttachVolume(_ context.Context, in AttachVolumeInput) (VolumeAttachment, error) {
	const op = "AttachVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return VolumeAttachment{}, err
	}
	v, ok := f.volumes[in.VolumeID]
	if !ok || v.State.Gone() {
		return VolumeAttachment{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume not found")
	}
	if v.State != VolumeAvailable {
		return VolumeAttachment{}, fakeError(op, 409, "Conflict", "volume is not available")
	}
	limit := f.AttachmentCap
	if limit == 0 {
		limit = MaxISCSIAttachmentsPerInstance
	}
	onInstance := 0
	for _, a := range f.attachments {
		if a.State == AttachmentDetached {
			continue
		}
		if a.VolumeID == in.VolumeID {
			return VolumeAttachment{}, fakeError(op, 409, "Conflict", "volume is already attached and is not shareable")
		}
		if a.InstanceID == in.InstanceID {
			onInstance++
		}
	}
	if onInstance >= limit {
		return VolumeAttachment{}, fakeError(op, 400, "LimitExceeded", "the instance has reached its volume attachment limit")
	}
	id := f.newID("volumeattachment")
	a := &fakeAttachment{
		VolumeAttachment: VolumeAttachment{
			ID: id, InstanceID: in.InstanceID, VolumeID: in.VolumeID, AvailabilityDomain: v.AvailabilityDomain,
			State: AttachmentAttaching, ReadOnly: in.ReadOnly,
			IQN: IQNPrefix + id, IPv4: fmt.Sprintf("169.254.2.%d", 2+f.seq%250), Port: 3260,
		},
		countdown: f.AttachAfter,
	}
	if a.countdown == 0 {
		a.State = AttachmentAttached
	}
	f.attachments[id] = a
	return answerOf(f, op, a.VolumeAttachment)
}

func (f *BlockFake) GetVolumeAttachment(_ context.Context, attachmentID string) (VolumeAttachment, error) {
	const op = "GetVolumeAttachment"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return VolumeAttachment{}, err
	}
	a, ok := f.attachments[attachmentID]
	if !ok {
		return VolumeAttachment{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume attachment not found")
	}
	if a.State == AttachmentAttaching || a.State == AttachmentDetaching {
		if a.countdown > 0 {
			a.countdown--
		}
		if a.countdown == 0 && a.State == AttachmentAttaching {
			a.State = AttachmentAttached
		} else if a.countdown == 0 {
			a.State = AttachmentDetached
		}
	}
	return a.VolumeAttachment, nil
}

func (f *BlockFake) ListVolumeAttachments(_ context.Context, filter AttachmentFilter) ([]VolumeAttachment, error) {
	const op = "ListVolumeAttachments"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return nil, err
	}
	var out []VolumeAttachment
	for _, a := range f.attachments {
		if (filter.VolumeID != "" && a.VolumeID != filter.VolumeID) ||
			(filter.InstanceID != "" && a.InstanceID != filter.InstanceID) {
			continue
		}
		out = append(out, a.VolumeAttachment)
	}
	slices.SortFunc(out, func(a, b VolumeAttachment) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *BlockFake) DetachVolume(_ context.Context, attachmentID string) error {
	const op = "DetachVolume"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return err
	}
	a, ok := f.attachments[attachmentID]
	if !ok {
		return fakeError(op, 404, "NotAuthorizedOrNotFound", "volume attachment not found")
	}
	if a.State == AttachmentDetaching || a.State == AttachmentDetached {
		return fakeError(op, 409, "Conflict", "volume attachment is "+string(a.State))
	}
	a.State, a.countdown = AttachmentDetaching, f.DetachAfter
	if a.countdown == 0 {
		a.State = AttachmentDetached
	}
	_, err := answerOf(f, op, struct{}{})
	return err
}

func (f *BlockFake) CreateVolumeBackup(_ context.Context, in CreateVolumeBackupInput) (VolumeBackup, error) {
	const op = "CreateVolumeBackup"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return VolumeBackup{}, err
	}
	if id, ok := f.replayed(op, in.RetryToken); ok {
		return answerOf(f, op, cloneBackup(*f.backups[id]))
	}
	v, ok := f.volumes[in.VolumeID]
	if !ok || v.State.Gone() {
		return VolumeBackup{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume not found")
	}
	id := f.newID("volumebackup")
	f.backups[id] = &VolumeBackup{
		ID: id, CompartmentID: v.CompartmentID, VolumeID: in.VolumeID, DisplayName: in.DisplayName,
		SizeGB: v.SizeGB, State: BackupCreating, FreeformTags: maps.Clone(in.FreeformTags), TimeCreated: time.Now().UTC(),
	}
	f.remember(op, in.RetryToken, id)
	return answerOf(f, op, cloneBackup(*f.backups[id]))
}

func (f *BlockFake) GetVolumeBackup(_ context.Context, backupID string) (VolumeBackup, error) {
	const op = "GetVolumeBackup"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return VolumeBackup{}, err
	}
	b, ok := f.backups[backupID]
	if !ok {
		return VolumeBackup{}, fakeError(op, 404, "NotAuthorizedOrNotFound", "volume backup not found")
	}
	return cloneBackup(*b), nil
}

func (f *BlockFake) ListVolumeBackups(_ context.Context, filter BackupFilter) ([]VolumeBackup, error) {
	const op = "ListVolumeBackups"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return nil, err
	}
	var out []VolumeBackup
	for _, b := range f.backups {
		if b.CompartmentID != filter.CompartmentID ||
			(filter.DisplayName != "" && b.DisplayName != filter.DisplayName) ||
			!hasTags(b.FreeformTags, filter.Tags) {
			continue
		}
		out = append(out, cloneBackup(*b))
	}
	return out, nil
}

func (f *BlockFake) DeleteVolumeBackup(_ context.Context, backupID string) error {
	const op = "DeleteVolumeBackup"
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(op); err != nil {
		return err
	}
	b, ok := f.backups[backupID]
	if !ok || b.State.Gone() {
		return fakeError(op, 404, "NotAuthorizedOrNotFound", "volume backup not found")
	}
	if b.State.InProgress() {
		return fakeError(op, 409, "Conflict", "Cannot delete Volume Backup "+backupID+" while in state Creating.")
	}
	b.State = BackupTerminated
	_, err := answerOf(f, op, struct{}{})
	return err
}

// answerOf returns out, or ErrReplyLost once if LoseReply staged op.
func answerOf[T any](f *BlockFake, op string, out T) (T, error) {
	if f.loseReply[op] {
		delete(f.loseReply, op)
		var zero T
		return zero, ErrReplyLost
	}
	return out, nil
}

// replayed finds the resource an earlier call with the same retry token made.
func (f *BlockFake) replayed(op, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	id, ok := f.tokens[op+"/"+token]
	return id, ok
}

func (f *BlockFake) remember(op, token, id string) {
	if token == "" {
		return
	}
	f.tokens[op+"/"+token] = id
	f.tokenOf[id] = token
}

func (f *BlockFake) newID(kind string) string {
	f.seq++
	return fmt.Sprintf("ocid1.%s.oc1..fake%06d", kind, f.seq)
}

func fakeError(op string, status int, code, message string) error {
	return newAPIError(op, status, code, message, "fake")
}

func cloneVolume(v BlockVolume) BlockVolume {
	v.FreeformTags = maps.Clone(v.FreeformTags)
	return v
}

func cloneBackup(b VolumeBackup) VolumeBackup {
	b.FreeformTags = maps.Clone(b.FreeformTags)
	return b
}
