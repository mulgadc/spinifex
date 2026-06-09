import { GetSessionTokenCommand, STSClient } from "@aws-sdk/client-sts"
import { HttpRequest } from "@smithy/protocol-http"

import {
  type AwsCredentialsInput,
  getCredentials,
  type SessionCredentials,
} from "./auth"

const AWS_REGION = "ap-southeast-2"

// SDK signs against the real backend host so the SigV4 signature includes the
// correct Host header; middleware rewrites the outgoing URL to the same-origin
// reverse proxy after signing. Mirrors awsClient.ts.
const AWSGW_SIGN_ENDPOINT = `${window.location.protocol}//localhost:9999`

// Session lifetime requested from STS GetSessionToken (12 hours).
const SESSION_DURATION_SECONDS = 43_200

function addProxyRewrite(client: STSClient): void {
  client.middlewareStack.add(
    (next) => async (args) => {
      if (HttpRequest.isInstance(args.request)) {
        args.request.hostname = window.location.hostname
        args.request.port = Number(window.location.port) || 443
        args.request.path = `/proxy/awsgw${args.request.path}`
      }
      return await next(args)
    },
    { step: "finalizeRequest", name: "proxyRewrite", override: true },
  )
}

function buildStsClient(credentials: {
  accessKeyId: string
  secretAccessKey: string
  sessionToken?: string
}): STSClient {
  const client = new STSClient({
    endpoint: AWSGW_SIGN_ENDPOINT,
    region: AWS_REGION,
    credentials,
  })
  addProxyRewrite(client)
  return client
}

// getStsClient builds an STS client from the stored session credentials, for
// authenticated calls such as GetCallerIdentity.
export function getStsClient(): STSClient {
  const credentials = getCredentials()
  if (!credentials) {
    throw new Error("AWS credentials not configured")
  }
  return buildStsClient({
    accessKeyId: credentials.accessKeyId,
    secretAccessKey: credentials.secretAccessKey,
    sessionToken: credentials.sessionToken,
  })
}

// exchangeForSession trades the user's long-lived credentials for short-lived
// STS session credentials via GetSessionToken. The long-lived secret signs only
// this one request and is never persisted.
export async function exchangeForSession(
  input: AwsCredentialsInput,
): Promise<SessionCredentials> {
  const client = buildStsClient(input)
  const result = await client.send(
    new GetSessionTokenCommand({ DurationSeconds: SESSION_DURATION_SECONDS }),
  )
  const creds = result.Credentials
  if (
    !creds?.AccessKeyId ||
    !creds.SecretAccessKey ||
    !creds.SessionToken ||
    !creds.Expiration
  ) {
    throw new Error("GetSessionToken returned incomplete credentials")
  }
  return {
    accessKeyId: creds.AccessKeyId,
    secretAccessKey: creds.SecretAccessKey,
    sessionToken: creds.SessionToken,
    expiration: creds.Expiration.toISOString(),
  }
}
