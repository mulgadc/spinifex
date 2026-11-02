import { vi } from "vitest"

type ClientGetter = Extract<
  keyof typeof import("@/lib/aws-client"),
  `get${string}Client`
>

// Accepts any vi.fn or typed fake, since the code under test only ever calls
// send with a command and awaits the result.
type Send = (command: never) => unknown

export type AwsClientsMock = Record<ClientGetter, () => { send: Send }> & {
  clearClients: () => void
}

// Keyed so the compiler flags a getter added to awsClient but missing here.
const CLIENT_GETTERS: Record<ClientGetter, true> = {
  getAcmClient: true,
  getBedrockAgentClient: true,
  getBedrockAgentRuntimeClient: true,
  getBedrockClient: true,
  getBedrockRuntimeClient: true,
  getEc2Client: true,
  getEcrClient: true,
  getEcsClient: true,
  getEksClient: true,
  getElbv2Client: true,
  getIamClient: true,
  getRdsClient: true,
  getRoute53Client: true,
  getS3Client: true,
}

// Builds the awsClient module for a vi.mock factory: every getter returns
// `{ send }` unless `overrides` names it. The factory runs on awsClient's first
// load, so hoist the sends and import this dynamically inside the factory.
export function mockAwsClients(
  send: Send,
  overrides: Partial<Record<ClientGetter, Send>> = {},
): AwsClientsMock {
  // SAFETY: the entries cover every key of CLIENT_GETTERS, which is keyed by ClientGetter.
  const getters = Object.fromEntries(
    Object.keys(CLIENT_GETTERS).map((name) => {
      // SAFETY: name is a key of CLIENT_GETTERS, whose keys are ClientGetter by its type.
      const client = { send: overrides[name as ClientGetter] ?? send }

      return [name, () => client]
    }),
  ) as Record<ClientGetter, () => { send: Send }>

  return { ...getters, clearClients: vi.fn() }
}
