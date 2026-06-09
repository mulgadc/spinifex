import { ACMClient } from "@aws-sdk/client-acm"
import { EC2Client } from "@aws-sdk/client-ec2"
import { EKSClient } from "@aws-sdk/client-eks"
import { ElasticLoadBalancingV2Client } from "@aws-sdk/client-elastic-load-balancing-v2"
import { IAMClient } from "@aws-sdk/client-iam"
import { S3Client } from "@aws-sdk/client-s3"
import { HttpRequest } from "@smithy/protocol-http"

import { getCredentials } from "./auth"

const AWS_REGION = "ap-southeast-2"

// SDK signs against the real backend host so the SigV4 signature includes
// the correct Host header value. Middleware rewrites the outgoing URL
// to route through the same-origin reverse proxy after signing is complete.
const AWSGW_SIGN_ENDPOINT = `${window.location.protocol}//localhost:9999`
const S3_SIGN_ENDPOINT = `${window.location.protocol}//localhost:8443`

// Cached singleton clients
let ec2Client: EC2Client | null = null
let eksClient: EKSClient | null = null
let elbv2Client: ElasticLoadBalancingV2Client | null = null
let acmClient: ACMClient | null = null
let iamClient: IAMClient | null = null
let s3Client: S3Client | null = null

export function getEc2Client(): EC2Client {
  if (!ec2Client) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    ec2Client = new EC2Client({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
    })
    ec2Client.middlewareStack.add(
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
  return ec2Client
}

export function getEksClient(): EKSClient {
  if (!eksClient) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    eksClient = new EKSClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
    })
    eksClient.middlewareStack.add(
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
  return eksClient
}

export function getElbv2Client(): ElasticLoadBalancingV2Client {
  if (!elbv2Client) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    elbv2Client = new ElasticLoadBalancingV2Client({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
    })
    elbv2Client.middlewareStack.add(
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
  return elbv2Client
}

export function getAcmClient(): ACMClient {
  if (!acmClient) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    acmClient = new ACMClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
    })
    acmClient.middlewareStack.add(
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
  return acmClient
}

export function getIamClient(): IAMClient {
  if (!iamClient) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    iamClient = new IAMClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
    })
    iamClient.middlewareStack.add(
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
  return iamClient
}

export function getS3Client(): S3Client {
  if (!s3Client) {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }
    s3Client = new S3Client({
      endpoint: S3_SIGN_ENDPOINT,
      region: AWS_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
      },
      forcePathStyle: true,
    })
    // Remove trailing slashes from request paths to fix compatibility with
    // path-style S3 endpoints where a trailing slash causes the request to
    // be interpreted as GetObject instead of ListObjects
    s3Client.middlewareStack.add(
      (next) => async (args) => {
        if (
          HttpRequest.isInstance(args.request) &&
          args.request.path.endsWith("/") &&
          args.request.path !== "/"
        ) {
          args.request.path = args.request.path.slice(0, -1)
        }
        return await next(args)
      },
      { step: "build", name: "removeTrailingSlash" },
    )
    s3Client.middlewareStack.add(
      (next) => async (args) => {
        if (HttpRequest.isInstance(args.request)) {
          args.request.hostname = window.location.hostname
          args.request.port = Number(window.location.port) || 443
          args.request.path = `/proxy/s3${args.request.path}`
        }
        return await next(args)
      },
      { step: "finalizeRequest", name: "proxyRewrite", override: true },
    )
  }
  return s3Client
}

// Call on logout to clear cached clients
export function clearClients(): void {
  ec2Client = null
  eksClient = null
  elbv2Client = null
  acmClient = null
  iamClient = null
  s3Client = null
}
