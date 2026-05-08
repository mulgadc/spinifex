import { Sha256 } from "@aws-crypto/sha256-browser"
import { HttpRequest } from "@smithy/protocol-http"
import { SignatureV4 } from "@smithy/signature-v4"

import type { AwsCredentials } from "./auth"

const AWS_REGION = "ap-southeast-2"
const GATEWAY_PORT = 9999

interface SignedFetchOptions {
  action: string
  credentials: AwsCredentials
  service?: string
}

export async function signedFetch<T>({
  action,
  credentials,
  service = "spinifex",
}: SignedFetchOptions): Promise<T> {
  const protocol = window.location.protocol.replace(":", "")
  const body = `Action=${action}`

  // Sign the request against the real backend (localhost:9999) so the
  // gateway's SigV4 verification sees the host value it expects.
  const request = new HttpRequest({
    method: "POST",
    protocol,
    hostname: "localhost",
    port: GATEWAY_PORT,
    path: "/",
    headers: {
      host: `localhost:${GATEWAY_PORT}`,
      "content-type": "application/x-www-form-urlencoded",
    },
    body,
  })

  const signer = new SignatureV4({
    credentials: {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
    },
    region: AWS_REGION,
    service,
    sha256: Sha256,
  })

  const signed = await signer.sign(request)

  const headers: Record<string, string> = {}
  for (const [key, value] of Object.entries(signed.headers)) {
    if (typeof value === "string") {
      headers[key] = value
    }
  }

  // Send the request through the same-origin reverse proxy instead of
  // directly to the gateway, eliminating cross-origin requests.
  const proxyUrl = `${window.location.protocol}//${window.location.host}/proxy/awsgw/`
  const response = await fetch(proxyUrl, {
    method: "POST",
    headers,
    body,
  })

  if (!response.ok) {
    const detail = await response.text().catch(() => "")
    throw new Error(
      `${action} failed: ${response.status}${detail ? ` - ${detail}` : ""}`,
    )
  }

  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- response.json() returns Promise<any>
  return await (response.json() as Promise<T>)
}
