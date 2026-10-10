import { getCredentials } from "@/lib/auth"
import { JSON_CONTENT_TYPE, signedProxyFetch } from "@/lib/signed-fetch"

const TARGET = "AmazonEC2ContainerServiceV20141113.ProvisionCapacity"

export interface ProvisionCapacityRequest {
  Cluster: string
  InstanceType?: string
  Count?: number
  SubnetID: string
  SecurityGroupID: string
  KeyName?: string
}

export interface ProvisionCapacityResponse {
  InstanceIDs?: string[]
}

// provisionCapacity calls the custom ProvisionCapacity gateway action. The AWS
// SDK has no command for it, so the JSON-1.1 request is SigV4-signed by hand
// and POSTed through the same-origin proxy.
export async function provisionCapacity(
  req: ProvisionCapacityRequest,
): Promise<ProvisionCapacityResponse> {
  const credentials = getCredentials()
  if (!credentials) {
    throw new Error("Not authenticated")
  }

  return await signedProxyFetch<ProvisionCapacityResponse>({
    label: "ProvisionCapacity",
    credentials,
    service: "ecs",
    contentType: JSON_CONTENT_TYPE,
    target: TARGET,
    body: JSON.stringify(req),
  })
}
