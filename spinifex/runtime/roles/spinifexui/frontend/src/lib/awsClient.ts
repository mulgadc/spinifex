import { ACMClient } from "@aws-sdk/client-acm"
import { BedrockClient } from "@aws-sdk/client-bedrock"
import { BedrockAgentClient } from "@aws-sdk/client-bedrock-agent"
import { BedrockAgentRuntimeClient } from "@aws-sdk/client-bedrock-agent-runtime"
import { BedrockRuntimeClient } from "@aws-sdk/client-bedrock-runtime"
import { EC2Client } from "@aws-sdk/client-ec2"
import { ECRClient } from "@aws-sdk/client-ecr"
import { ECSClient } from "@aws-sdk/client-ecs"
import { EKSClient } from "@aws-sdk/client-eks"
import { ElasticLoadBalancingV2Client } from "@aws-sdk/client-elastic-load-balancing-v2"
import { IAMClient } from "@aws-sdk/client-iam"
import { RDSClient } from "@aws-sdk/client-rds"
import { Route53Client } from "@aws-sdk/client-route-53"
import { S3Client } from "@aws-sdk/client-s3"
import { XhrHttpHandler } from "@aws-sdk/xhr-http-handler"
import { HttpRequest } from "@smithy/protocol-http"

import { getCredentials } from "./auth"
import { getRegion } from "./cluster-config"

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
let ecrClient: ECRClient | null = null
let ecsClient: ECSClient | null = null
let rdsClient: RDSClient | null = null
let route53Client: Route53Client | null = null
let bedrockAgentClient: BedrockAgentClient | null = null
let bedrockAgentRuntimeClient: BedrockAgentRuntimeClient | null = null
let bedrockClient: BedrockClient | null = null
let bedrockRuntimeClient: BedrockRuntimeClient | null = null

export function getEc2Client(): EC2Client {
  if (!ec2Client) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    ec2Client = new EC2Client({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
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
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
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
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
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
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
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
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
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

export function getEcrClient(): ECRClient {
  if (!ecrClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    ecrClient = new ECRClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    ecrClient.middlewareStack.add(
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

  return ecrClient
}

export function getEcsClient(): ECSClient {
  if (!ecsClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    ecsClient = new ECSClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    ecsClient.middlewareStack.add(
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

  return ecsClient
}

export function getRdsClient(): RDSClient {
  if (!rdsClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    rdsClient = new RDSClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    rdsClient.middlewareStack.add(
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

  return rdsClient
}

// Route 53 is a global service: the SDK signs it for us-east-1, which the
// gateway accepts alongside its own region.
const ROUTE53_SIGNING_REGION = "us-east-1"

export function getRoute53Client(): Route53Client {
  if (!route53Client) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    route53Client = new Route53Client({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: ROUTE53_SIGNING_REGION,
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    route53Client.middlewareStack.add(
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

  return route53Client
}

function newS3Client(requestHandler?: XhrHttpHandler): S3Client {
  const credentials = getCredentials()

  if (!credentials) {
    throw new Error("AWS credentials not configured")
  }

  const client = new S3Client({
    endpoint: S3_SIGN_ENDPOINT,
    region: getRegion(),
    credentials: {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
      sessionToken: credentials.sessionToken,
    },
    forcePathStyle: true,
    requestHandler,
  })

  // Remove trailing slashes from request paths to fix compatibility with
  // path-style S3 endpoints where a trailing slash causes the request to
  // be interpreted as GetObject instead of ListObjects
  client.middlewareStack.add(
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
  client.middlewareStack.add(
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

  return client
}

export function getS3Client(): S3Client {
  s3Client ??= newS3Client()

  return s3Client
}

// A client of its own per upload: XHR reports bytes sent as they go, and the
// upload's progress listener hears every request on its client's handler.
export function createS3UploadClient(): S3Client {
  return newS3Client(new XhrHttpHandler({}))
}

export function getBedrockAgentClient(): BedrockAgentClient {
  if (!bedrockAgentClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    bedrockAgentClient = new BedrockAgentClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    bedrockAgentClient.middlewareStack.add(
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

  return bedrockAgentClient
}

export function getBedrockAgentRuntimeClient(): BedrockAgentRuntimeClient {
  if (!bedrockAgentRuntimeClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    bedrockAgentRuntimeClient = new BedrockAgentRuntimeClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    bedrockAgentRuntimeClient.middlewareStack.add(
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

  return bedrockAgentRuntimeClient
}

export function getBedrockClient(): BedrockClient {
  if (!bedrockClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    bedrockClient = new BedrockClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    bedrockClient.middlewareStack.add(
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

  return bedrockClient
}

export function getBedrockRuntimeClient(): BedrockRuntimeClient {
  if (!bedrockRuntimeClient) {
    const credentials = getCredentials()

    if (!credentials) {
      throw new Error("AWS credentials not configured")
    }

    bedrockRuntimeClient = new BedrockRuntimeClient({
      endpoint: AWSGW_SIGN_ENDPOINT,
      region: getRegion(),
      credentials: {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        sessionToken: credentials.sessionToken,
      },
    })
    bedrockRuntimeClient.middlewareStack.add(
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

  return bedrockRuntimeClient
}

// Call on logout to clear cached clients
export function clearClients(): void {
  ec2Client = null
  eksClient = null
  elbv2Client = null
  acmClient = null
  iamClient = null
  s3Client = null
  ecrClient = null
  ecsClient = null
  rdsClient = null
  route53Client = null
  bedrockAgentClient = null
  bedrockAgentRuntimeClient = null
  bedrockClient = null
  bedrockRuntimeClient = null
}
