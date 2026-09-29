// Package resource is the envelope a persisted control-plane record lives in:
// what an operator asked for, what a node observed, and the identity and
// lifecycle bookkeeping that is common to both. It is a leaf, importing nothing
// from spinifex, so any layer can name a record without pulling in its service.
package resource

import (
	"slices"
	"time"
)

// Object separates the two halves of a record by write authority. Spec is
// written only by the API layer, under CAS against the KV. Status is written
// only by the node that owns the resource, to its local state file first and
// then to the KV best-effort. Nothing in Go can enforce that, so the split is a
// convention the type makes visible rather than a guarantee it provides.
type Object[Spec, Status any] struct {
	Metadata Metadata `json:"metadata"`
	Spec     Spec     `json:"spec"`
	Status   Status   `json:"status"`
}

// Metadata is the identity and lifecycle bookkeeping every record carries,
// independent of what it describes.
//
// ARNs are deliberately absent: the arn package already builds them, and the
// per-service formats are not uniform enough for one method over this struct.
type Metadata struct {
	// Name is the AWS-visible identifier — an instance id, a cluster name.
	Name string `json:"name"`

	// UID distinguishes this resource from an earlier one that carried the
	// same Name, so an OwnerRef cannot be satisfied by a replacement.
	UID string `json:"uid,omitempty"`

	AccountID string `json:"account_id,omitempty"`
	Region    string `json:"region,omitempty"`

	// Generation counts spec changes; ObservedGeneration is the generation the
	// owning node has finished acting on. Equal means the node has caught up,
	// which is what a caller polls instead of the resource's own fields.
	Generation         int64 `json:"generation,omitempty"`
	ObservedGeneration int64 `json:"observed_generation,omitempty"`

	// OwnerRefs point at the resources this one is a dependent of. Deleting an
	// owner cascades to its dependents.
	OwnerRefs []OwnerRef `json:"owner_refs,omitempty"`

	// Finalizers hold a delete open. A resource marked for deletion is not
	// removed until every finalizer has been cleared by whatever registered it.
	Finalizers []string `json:"finalizers,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`

	// DeletionTimestamp marks the resource for deletion. It is set before the
	// resource's own state has caught up, so it is the signal to read during
	// the window where that state still says the resource is live.
	DeletionTimestamp *time.Time `json:"deletion_timestamp,omitempty"`
}

// OwnerRef identifies the resource a dependent belongs to. Kind is the owner's
// resource type as the service names it ("instance", "cluster").
type OwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

// MarkedForDeletion reports whether a delete has been accepted for this
// resource, whether or not its status has caught up yet.
func (m *Metadata) MarkedForDeletion() bool {
	return m.DeletionTimestamp != nil
}

// MarkForDeletion stamps the deletion time, and reports false if one was
// already stamped. Deleting twice must not move the timestamp: finalizer
// timeouts are measured from it.
func (m *Metadata) MarkForDeletion(now time.Time) bool {
	if m.DeletionTimestamp != nil {
		return false
	}
	t := now.UTC()
	m.DeletionTimestamp = &t
	return true
}

func (m *Metadata) HasFinalizer(name string) bool {
	return slices.Contains(m.Finalizers, name)
}

// AddFinalizer registers a finalizer, reporting false if it was already
// present. Registering after the resource is marked for deletion is refused:
// the delete has already been evaluated against the finalizers it had.
func (m *Metadata) AddFinalizer(name string) bool {
	if m.MarkedForDeletion() || m.HasFinalizer(name) {
		return false
	}
	m.Finalizers = append(m.Finalizers, name)
	return true
}

// RemoveFinalizer clears a finalizer, reporting false if it was not present.
// The resource is collectable once the last one is gone.
func (m *Metadata) RemoveFinalizer(name string) bool {
	idx := slices.Index(m.Finalizers, name)
	if idx < 0 {
		return false
	}
	m.Finalizers = slices.Delete(m.Finalizers, idx, idx+1)
	if len(m.Finalizers) == 0 {
		m.Finalizers = nil
	}
	return true
}

// Collectable reports whether a marked resource can now be removed. A resource
// nobody asked to delete is never collectable.
func (m *Metadata) Collectable() bool {
	return m.MarkedForDeletion() && len(m.Finalizers) == 0
}

// OwnerOfKind returns the owner reference of the given kind. A resource has at
// most one owner per kind; a second is a bug in the caller, not a list.
func (m *Metadata) OwnerOfKind(kind string) (OwnerRef, bool) {
	for _, ref := range m.OwnerRefs {
		if ref.Kind == kind {
			return ref, true
		}
	}
	return OwnerRef{}, false
}

// AddOwnerRef records an owner, reporting false if one of that kind is already
// recorded. Replacing an owner is a distinct operation from acquiring one, so
// it is not silently allowed here.
func (m *Metadata) AddOwnerRef(ref OwnerRef) bool {
	if _, ok := m.OwnerOfKind(ref.Kind); ok {
		return false
	}
	m.OwnerRefs = append(m.OwnerRefs, ref)
	return true
}

// NeedsSync reports whether the owning node has yet to act on the current spec.
func (m *Metadata) NeedsSync() bool {
	return m.Generation != m.ObservedGeneration
}

// MutateSpec applies a spec change and bumps Generation. Going through this
// rather than assigning to Spec is what keeps Generation honest, and therefore
// what makes NeedsSync mean anything.
func (o *Object[Spec, Status]) MutateSpec(fn func(*Spec)) {
	fn(&o.Spec)
	o.Metadata.Generation++
}

// ObserveGeneration records that the owning node has finished acting on the
// spec it read. Called by the node after a successful reconcile, never by the
// API layer.
func (o *Object[Spec, Status]) ObserveGeneration(generation int64) {
	o.Metadata.ObservedGeneration = generation
}

// NeedsSync reports whether the owning node has yet to act on the current spec.
func (o *Object[Spec, Status]) NeedsSync() bool {
	return o.Metadata.NeedsSync()
}
