package vultr

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fake must agree with the client on every outcome the client's captured
// responses pin, or engine tests built on it prove nothing about Vultr.

func TestFakeFidelity_ReattachToHolderSucceeds(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/attach": {{http.StatusBadRequest, "attach_already_attached.json"}},
		"GET /reserved-ips/" + capturedID:              {{http.StatusOK, "get_attached.json"}},
	})
	require.NoError(t, c.AttachReservedIP(context.Background(), capturedID, capturedInstance))

	f := NewFake("ord", 0, capturedInstance)
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)
	require.NoError(t, f.AttachReservedIP(context.Background(), r.ID, capturedInstance))
	require.NoError(t, f.AttachReservedIP(context.Background(), r.ID, capturedInstance))
}

func TestFakeFidelity_AttachElsewhereIsHolderBusy(t *testing.T) {
	f := NewFake("ord", 0, "a", "b")
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)
	require.NoError(t, f.AttachReservedIP(context.Background(), r.ID, "a"))
	require.ErrorIs(t, f.AttachReservedIP(context.Background(), r.ID, "b"), ErrHolderBusy)
}

func TestFakeFidelity_DeleteAttachedSucceeds(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"DELETE /reserved-ips/" + capturedID: {{http.StatusNoContent, ""}},
	})
	require.NoError(t, c.DeleteReservedIP(context.Background(), capturedID))

	f := NewFake("ord", 0, "a")
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)
	require.NoError(t, f.AttachReservedIP(context.Background(), r.ID, "a"))
	require.NoError(t, f.DeleteReservedIP(context.Background(), r.ID))
	assert.Empty(t, f.IDs())
}

func TestFakeFidelity_DeleteAttachedRefusedDetaches(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"DELETE /reserved-ips/" + capturedID: {
			{http.StatusBadRequest, "delete_attached_refused.json"},
			{http.StatusBadRequest, "delete_attached_refused.json"},
			{http.StatusBadRequest, "delete_attached_refused.json"},
		},
	})
	err := c.DeleteReservedIP(context.Background(), capturedID)
	require.ErrorIs(t, err, errDetachRefused, "a refusal that outlasts the retries is still an error")

	f := NewFake("ord", 0, "a")
	f.RefuseDeleteAttached = true
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)
	require.NoError(t, f.AttachReservedIP(context.Background(), r.ID, "a"))
	require.Error(t, f.DeleteReservedIP(context.Background(), r.ID))
	assert.Empty(t, f.HolderOf(r.ID), "the refused delete still detached the address")
	require.NoError(t, f.DeleteReservedIP(context.Background(), r.ID))
}

func TestFakeFidelity_DetachUnattachedSucceeds(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/detach": {{http.StatusNoContent, ""}},
	})
	require.NoError(t, c.DetachReservedIP(context.Background(), capturedID))

	f := NewFake("ord", 0)
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)
	require.NoError(t, f.DetachReservedIP(context.Background(), r.ID))
}

func TestFakeFidelity_MissingIsNotFound(t *testing.T) {
	const missing = "00000000-0000-0000-0000-000000000000"
	_, c := newFakeAPI(t, map[string][]reply{
		"GET /reserved-ips/" + missing:                 {{http.StatusNotFound, "get_missing.json"}},
		"DELETE /reserved-ips/" + missing:              {{http.StatusNotFound, "delete_missing.json"}},
		"POST /reserved-ips/" + missing + "/detach":    {{http.StatusNotFound, "delete_missing.json"}},
		"POST /reserved-ips/" + capturedID + "/attach": {{http.StatusNotFound, "attach_missing_instance.json"}},
	})
	f := NewFake("ord", 0)
	r, err := f.CreateReservedIP(context.Background(), "l")
	require.NoError(t, err)

	_, err = c.GetReservedIP(context.Background(), missing)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.GetReservedIP(context.Background(), missing)
	require.ErrorIs(t, err, ErrNotFound)

	require.ErrorIs(t, c.DeleteReservedIP(context.Background(), missing), ErrNotFound)
	require.ErrorIs(t, f.DeleteReservedIP(context.Background(), missing), ErrNotFound)

	require.ErrorIs(t, c.DetachReservedIP(context.Background(), missing), ErrNotFound)
	require.ErrorIs(t, f.DetachReservedIP(context.Background(), missing), ErrNotFound)

	require.ErrorIs(t, c.AttachReservedIP(context.Background(), capturedID, missing), ErrNotFound)
	require.ErrorIs(t, f.AttachReservedIP(context.Background(), r.ID, missing), ErrNotFound)
}

func TestFake_QuotaIsQuotaExceeded(t *testing.T) {
	f := NewFake("ord", 1)
	_, err := f.CreateReservedIP(context.Background(), "a")
	require.NoError(t, err)
	_, err = f.CreateReservedIP(context.Background(), "b")
	require.ErrorIs(t, err, ErrQuotaExceeded)
}
