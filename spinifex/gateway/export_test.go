package gateway

import "fmt"

// AdvertisedEndpoint exposes the gateway's own dialable base URL to the
// external test package.
func (gw *GatewayConfig) AdvertisedEndpoint() string { return gw.advertisedEndpoint() }

// DispatchEC2Action runs an action through the parse-then-dispatch entry
// EC2_Request uses, without an *http.Request.
func DispatchEC2Action(gw *GatewayConfig, action string, query map[string]string, accountID string) ([]byte, error) {
	h, ok := ec2Actions[action]
	if !ok {
		return nil, fmt.Errorf("no EC2 action %q", action)
	}
	input, err := h.parse(query)
	if err != nil {
		return nil, err
	}
	return h.dispatch(action, input, gw, accountID, nil)
}
