package vm

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
)

// IMDSDatapathReconcileInterval is how often running guests' IMDS datapaths are
// checked against what was installed. It bounds how long a guest stays dark
// after ovs-vswitchd restarts. A var so tests can shorten it.
var IMDSDatapathReconcileInterval = 10 * time.Second

// imdsRepairConcurrency bounds how many datapaths one pass re-installs at once.
// An ovs-vswitchd restart drops every guest's flows together, so serial repairs
// would keep the last guest dark for the sum of everyone else's install.
const imdsRepairConcurrency = 8

// imdsStateReadTimeout and imdsRepairTimeout bound a pass's read and each repair.
// An ovs-vswitchd that dies mid-repair would otherwise hang the repair while it
// holds the ENI lock, blocking that guest's stop or terminate. Vars for tests.
var (
	imdsStateReadTimeout = 10 * time.Second
	imdsRepairTimeout    = 30 * time.Second
)

// imdsDatapathTarget is a running guest whose primary ENI has an IMDS datapath,
// copied out of the manager lock with everything a re-install needs.
type imdsDatapathTarget struct {
	instanceID string
	eniID      string
	mac        string
	subnetID   string
}

// StartIMDSDatapathReconcile re-installs the IMDS datapath of any running guest
// that has lost part of it. br-imds is fail-mode=secure, so a guest whose flows
// are gone has no network at all. Runs once now, then every interval.
func (m *Manager) StartIMDSDatapathReconcile(ctx context.Context) {
	if m.deps.NetworkPlumber == nil {
		return
	}
	go func() {
		m.reconcileIMDSDatapaths(ctx)
		ticker := time.NewTicker(IMDSDatapathReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.reconcileIMDSDatapaths(ctx)
			}
		}
	}()
}

// reconcileIMDSDatapaths runs one pass: read every running guest's datapath in
// one query, then repair each one that is not intact.
func (m *Manager) reconcileIMDSDatapaths(ctx context.Context) {
	targets := m.imdsDatapathTargets()
	if len(targets) == 0 {
		return
	}
	eniIDs := make([]string, len(targets))
	for i, t := range targets {
		eniIDs[i] = t.eniID
	}
	readCtx, cancel := context.WithTimeout(ctx, imdsStateReadTimeout)
	states, err := m.deps.NetworkPlumber.IMDSDatapathStates(readCtx, eniIDs)
	cancel()
	if err != nil {
		slog.Warn("IMDS: datapath state read failed; skipping reconcile pass", "err", err)
		return
	}
	sem := make(chan struct{}, imdsRepairConcurrency)
	var wg sync.WaitGroup
	for _, t := range imdsDatapathsToRepair(targets, states) {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			m.repairIMDSDatapath(ctx, t, states[t.eniID])
		})
	}
	wg.Wait()
}

// imdsDatapathTargets returns the running guests attachPrimaryIMDSDatapath
// gave a datapath: those with a primary ENI and a subnet.
func (m *Manager) imdsDatapathTargets() []imdsDatapathTarget {
	var targets []imdsDatapathTarget
	m.View(func(vms map[string]*VM) {
		for _, v := range vms {
			if t, ok := imdsTargetOf(v); ok {
				targets = append(targets, t)
			}
		}
	})
	return targets
}

// imdsTargetOf returns v as a repair target, or false if it should have no
// datapath right now. The caller holds the manager lock.
func imdsTargetOf(v *VM) (imdsDatapathTarget, bool) {
	if v.Status != StateRunning || v.ENIId == "" || v.Instance == nil {
		return imdsDatapathTarget{}, false
	}
	subnetID := aws.StringValue(v.Instance.SubnetId)
	if subnetID == "" {
		return imdsDatapathTarget{}, false
	}
	return imdsDatapathTarget{instanceID: v.ID, eniID: v.ENIId, mac: v.ENIMac, subnetID: subnetID}, true
}

// imdsDatapathsToRepair picks the targets whose datapath is not intact. An ENI
// missing from states was not read, so it is left alone rather than repaired.
func imdsDatapathsToRepair(targets []imdsDatapathTarget, states map[string]IMDSDatapathState) []imdsDatapathTarget {
	var out []imdsDatapathTarget
	for _, t := range targets {
		if s, ok := states[t.eniID]; ok && !s.Intact() {
			out = append(out, t)
		}
	}
	return out
}

// repairIMDSDatapath re-installs one guest's datapath under its ENI lock, so it
// cannot interleave with an attach or a teardown. The guest is re-checked under
// the lock because a teardown may have finished since the pass read it.
func (m *Manager) repairIMDSDatapath(ctx context.Context, t imdsDatapathTarget, s IMDSDatapathState) {
	unlock := m.imdsLocks.lock(t.eniID)
	defer unlock()

	if !m.stillIMDSTarget(t) {
		return
	}
	repairCtx, cancel := context.WithTimeout(ctx, imdsRepairTimeout)
	defer cancel()
	err := m.deps.NetworkPlumber.RepairIMDSDatapath(repairCtx, t.eniID, t.mac, t.subnetID)
	switch {
	case errors.Is(err, ErrIMDSTapDetached):
		slog.Debug("IMDS: tap left br-imds before repair; skipping",
			"instance", t.instanceID, "eni", t.eniID)
	case err != nil:
		slog.Error("IMDS: per-tap datapath repair failed; guest has no network until it succeeds",
			"instance", t.instanceID, "eni", t.eniID, "flows_found", s.FlowsFound,
			"flows_expected", s.FlowsExpected, "endpoint_ready", s.EndpointReady, "err", err)
	default:
		slog.Warn("IMDS: per-tap datapath was lost; re-installed",
			"instance", t.instanceID, "eni", t.eniID, "flows_found", s.FlowsFound,
			"flows_expected", s.FlowsExpected, "endpoint_ready", s.EndpointReady)
	}
}

// stillIMDSTarget reports whether t is still the same running guest with the
// same ENI, MAC and subnet the pass read.
func (m *Manager) stillIMDSTarget(t imdsDatapathTarget) bool {
	var ok bool
	m.View(func(vms map[string]*VM) {
		v, found := vms[t.instanceID]
		if !found {
			return
		}
		cur, isTarget := imdsTargetOf(v)
		ok = isTarget && cur == t
	})
	return ok
}

// keyedMutex serialises work per key. Entries are dropped once unheld, so it
// does not grow with every ENI the node has ever seen. The zero value is ready.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until key is free and returns the function that releases it.
func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
