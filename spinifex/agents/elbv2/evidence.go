package elbv2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	imdsWait       = time.Second
	imdsRetryTicks = 6
)

// imdsBase is the in-VM metadata endpoint (a variable so tests can point it at a fake).
var imdsBase = "http://169.254.169.254/latest"

// dataPathEvidence is the optional part of a heartbeat: which VM the agent runs
// in and what each data NIC's gateway probe saw. An empty value adds nothing,
// which is what an agent without a netcfg blob sends.
type dataPathEvidence struct {
	InstanceID string
	NICs       []NICReport
}

// addTo encodes the evidence as additional heartbeat query parameters.
func (e dataPathEvidence) addTo(params url.Values) {
	if e.InstanceID != "" {
		params.Set("InstanceID", e.InstanceID)
	}
	for i, n := range e.NICs {
		p := "NICs.member." + strconv.Itoa(i+1) + "."
		params.Set(p+"MAC", n.MAC)
		params.Set(p+"ProbeOK", strconv.FormatBool(n.ProbeOK))
		params.Set(p+"ConsecutiveFailures", strconv.FormatInt(n.ConsecutiveFailures, 10))
		params.Set(p+"RxPackets", strconv.FormatInt(n.RxPackets, 10))
		params.Set(p+"TxPackets", strconv.FormatInt(n.TxPackets, 10))
		params.Set(p+"RxDropped", strconv.FormatInt(n.RxDropped, 10))
		params.Set(p+"TxDropped", strconv.FormatInt(n.TxDropped, 10))
	}
}

// dataPathEvidence gathers the probe results and instance ID for one heartbeat.
// Neither step can fail the heartbeat: missing evidence is simply omitted.
func (a *Agent) dataPathEvidence() dataPathEvidence {
	nics := a.probeNICs()
	if len(nics) == 0 {
		return dataPathEvidence{}
	}
	if a.instanceID == "" {
		a.lookupInstanceID()
	}
	return dataPathEvidence{InstanceID: a.instanceID, NICs: nics}
}

// lookupInstanceID asks for the instance ID without ever holding up a tick: the
// call is bounded by imdsWait and, after a failure, retried only every
// imdsRetryTicks ticks.
func (a *Agent) lookupInstanceID() {
	if a.idSkip > 0 {
		a.idSkip--
		return
	}
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		id, err := a.instanceIDFn()
		ch <- result{id, err}
	}()
	timer := time.NewTimer(imdsWait)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err == nil && r.id != "" {
			a.instanceID = r.id
			return
		}
	case <-timer.C:
	}
	a.idSkip = imdsRetryTicks - 1
}

// fetchInstanceID reads the instance ID from the in-VM metadata service. The
// token request and the read share one deadline.
func fetchInstanceID() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), imdsWait)
	defer cancel()
	c := &http.Client{}

	var token string
	if req, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsBase+"/api/token", nil); err == nil {
		req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "60")
		if resp, err := c.Do(req); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				token = strings.TrimSpace(string(b))
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsBase+"/meta-data/instance-id", nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-Aws-Ec2-Metadata-Token", token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("instance-id: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("instance-id: empty")
	}
	return id, nil
}
