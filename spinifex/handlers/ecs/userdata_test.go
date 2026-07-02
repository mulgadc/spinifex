package handlers_ecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildContainerInstanceUserData(t *testing.T) {
	const caPEM = "-----BEGIN CERTIFICATE-----\nMIIBfakefakefake\n-----END CERTIFICATE-----"
	udata := buildContainerInstanceUserData(containerInstanceUserDataInput{
		GatewayURL:    "https://10.0.0.1:9999",
		GatewayCACert: caPEM,
		Region:        "ap-southeast-2",
		ClusterName:   "web",
	})

	assert.Contains(t, udata, "#cloud-config")
	assert.Contains(t, udata, "ECS_GATEWAY_URL=https://10.0.0.1:9999")
	assert.Contains(t, udata, "ECS_GATEWAY_CA="+ecsGatewayCAPath)
	assert.Contains(t, udata, "ECS_REGION=ap-southeast-2")
	assert.Contains(t, udata, "ECS_CLUSTER=web")

	// The CA PEM is written inline under the gateway-ca.pem file.
	assert.Contains(t, udata, "path: "+ecsGatewayCAPath)
	assert.Contains(t, udata, "-----BEGIN CERTIFICATE-----")
	assert.Contains(t, udata, "-----END CERTIFICATE-----")

	// No credentials ever touch the instance; the agent uses IMDS role creds.
	assert.NotContains(t, udata, "ECS_ACCESS_KEY")
	assert.NotContains(t, udata, "ECS_SECRET_KEY")
}
