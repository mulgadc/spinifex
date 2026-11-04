//go:build lambdatestlink

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
)

const entryGrant = "test-grant"

// testGrant is the grant the host launched this VM under.
type testGrant struct {
	grantID, attemptID, environmentID string
	generation                        uint64
}

// readGrant reads the test-grant fw_cfg entry: grant ID, attempt ID,
// environment ID and generation, one per line.
func readGrant(fwCfgDir string) (testGrant, error) {
	b, err := os.ReadFile(filepath.Join(fwCfgDir, entryGrant, "raw"))
	if err != nil {
		return testGrant{}, fmt.Errorf("read %s: %w", entryGrant, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 4 {
		return testGrant{}, errors.New(entryGrant + " entry malformed")
	}
	g := testGrant{grantID: lines[0], attemptID: lines[1], environmentID: lines[2]}
	g.generation, err = strconv.ParseUint(lines[3], 10, 64)
	if err != nil || g.generation == 0 || g.grantID == "" || g.attemptID == "" || g.environmentID == "" {
		return testGrant{}, errors.New(entryGrant + " entry malformed")
	}
	return g, nil
}

// checkBinding refuses a run unless the grant names the authenticated environment
// generation, the request carries that grant and its deadline is still ahead. The
// clock check is a local guard only: it cannot detect skew or a superseded grant.
func checkBinding(scope lambdav1.Scope, g testGrant, req hostlink.Request, now time.Time) error {
	parts := strings.Split(string(scope), "/")
	if len(parts) != 5 {
		return errors.New("authenticated scope malformed")
	}
	if parts[3] != g.environmentID || parts[4] != strconv.FormatUint(g.generation, 10) {
		return errors.New("grant is not for the authenticated environment generation")
	}
	if req.GrantID != g.grantID || req.AttemptID != g.attemptID {
		return errors.New("request does not carry this VM's grant")
	}
	if req.DeadlineMs <= 0 {
		return errors.New("request has no deadline")
	}
	if !now.Before(time.UnixMilli(req.DeadlineMs)) {
		return errors.New("request deadline already passed")
	}
	return nil
}
