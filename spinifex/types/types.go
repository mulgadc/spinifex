// Package types holds the request, response and event types shared across
// Spinifex components over NATS.
package types

// IGWEvent is published on vpc.igw-attach / vpc.igw-detach.
type IGWEvent struct {
	InternetGatewayId string `json:"internet_gateway_id"`
	VpcId             string `json:"vpc_id"`
}
