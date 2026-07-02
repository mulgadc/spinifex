import { Sha256 } from "@aws-crypto/sha256-browser"
import { HttpRequest } from "@smithy/protocol-http"
import { SignatureV4 } from "@smithy/signature-v4"

import { getCredentials } from "@/lib/auth"

const AWS_REGION = "ap-southeast-2"
const GATEWAY_PORT = 9999
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

// jsonErrorMessage extracts a human-readable message from a JSON-1.1 error
// body. Returns null when the body is not JSON or carries no message.
function jsonErrorMessage(body: string): string | null {
  try {
    const parsed: unknown = JSON.parse(body)
    if (parsed && typeof parsed === "object") {
      if ("message" in parsed && typeof parsed.message === "string") {
        return parsed.message
      }
      if ("Message" in parsed && typeof parsed.Message === "string") {
        return parsed.Message
      }
    }
  } catch {
    // Non-JSON body.
  }
  return null
}

// provisionCapacity calls the custom ProvisionCapacity gateway action. The AWS
// SDK has no command for it, so the JSON-1.1 request is SigV4-signed by hand,
// mirroring signed-fetch.ts, and POSTed through the same-origin proxy.
export async function provisionCapacity(
  req: ProvisionCapacityRequest,
): Promise<ProvisionCapacityResponse> {
  const credentials = getCredentials()
  if (!credentials) {
    throw new Error("Not authenticated")
  }

  const protocol = window.location.protocol.replace(":", "")
  const body = JSON.stringify(req)

  // Headers are set before signing so they are part of the signature, matching
  // the gateway's SigV4 verification expectations.
  const request = new HttpRequest({
    method: "POST",
    protocol,
    hostname: "localhost",
    port: GATEWAY_PORT,
    path: "/",
    headers: {
      host: `localhost:${GATEWAY_PORT}`,
      "content-type": "application/x-amz-json-1.1",
      "x-amz-target": TARGET,
    },
    body,
  })

  const signer = new SignatureV4({
    credentials: {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
      sessionToken: credentials.sessionToken,
    },
    region: AWS_REGION,
    service: "ecs",
    sha256: Sha256,
  })

  const signed = await signer.sign(request)

  const headers: Record<string, string> = {}
  for (const [key, value] of Object.entries(signed.headers)) {
    if (typeof value === "string") {
      headers[key] = value
    }
  }

  const proxyUrl = `${window.location.protocol}//${window.location.host}/proxy/awsgw/`
  const response = await fetch(proxyUrl, {
    method: "POST",
    headers,
    body,
  })

  if (!response.ok) {
    const detail = await response.text().catch(() => "")
    const summary = jsonErrorMessage(detail) ?? detail
    throw new Error(
      `ProvisionCapacity failed: ${response.status}${summary ? ` - ${summary}` : ""}`,
    )
  }

  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- response.json() returns Promise<any>
  return await (response.json() as Promise<ProvisionCapacityResponse>)
}
