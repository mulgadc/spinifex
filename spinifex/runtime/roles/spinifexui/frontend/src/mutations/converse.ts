import {
  ConverseCommand,
  type ConverseCommandInput,
  type Message,
  type TokenUsage,
} from "@aws-sdk/client-bedrock-runtime"
import { useMutation } from "@tanstack/react-query"

import { getBedrockRuntimeClient } from "@/lib/awsClient"

export interface ConverseInferenceConfig {
  temperature?: number
  topP?: number
  maxTokens?: number
}

export interface ConverseParams {
  modelId: string
  messages: Message[]
  system?: string
  inferenceConfig?: ConverseInferenceConfig
  guardrailIdentifier?: string
  guardrailVersion?: string
}

// The ConverseCommand actually sent, exposed alongside the outcome so the
// page's raw-request panel and copy-as-CLI can show exactly what went over
// the wire without rebuilding it themselves.
export interface ConverseOutcome {
  request: ConverseCommandInput
  result:
    | { status: "ok"; message: Message; usage: TokenUsage }
    | { status: "warming-up" }
}

// A cold model rejects Converse with one of these until it finishes loading.
// The SDK models them as thrown errors, so this is the one place that turns
// them into a normal (non-throwing) outcome the page can render as a turn.
const WARMING_UP_ERROR_NAMES = new Set([
  "ModelNotReadyException",
  "ServiceUnavailableException",
])

// oxlint-disable-next-line anti-slop/no-unknown-parameters -- classifies arbitrary caught values
export function isWarmingUpError(error: unknown): boolean {
  return error instanceof Error && WARMING_UP_ERROR_NAMES.has(error.name)
}

export function buildConverseInput(
  params: ConverseParams,
): ConverseCommandInput {
  const input: ConverseCommandInput = {
    modelId: params.modelId,
    messages: params.messages,
  }
  if (params.system !== undefined && params.system.trim().length > 0) {
    input.system = [{ text: params.system }]
  }
  if (params.inferenceConfig) {
    input.inferenceConfig = params.inferenceConfig
  }
  if (
    params.guardrailIdentifier !== undefined &&
    params.guardrailIdentifier.length > 0 &&
    params.guardrailVersion !== undefined &&
    params.guardrailVersion.length > 0
  ) {
    input.guardrailConfig = {
      guardrailIdentifier: params.guardrailIdentifier,
      guardrailVersion: params.guardrailVersion,
    }
  }
  return input
}

// The only function that touches ConverseCommand. A later ConverseStream swap
// edits this file alone: the request/outcome shape below is already built so
// the page mutates the last assistant turn in place rather than replacing it.
export async function sendConverse(
  params: ConverseParams,
): Promise<ConverseOutcome> {
  const request = buildConverseInput(params)
  try {
    const response = await getBedrockRuntimeClient().send(
      new ConverseCommand(request),
    )
    const message = response.output?.message
    if (!message) {
      throw new Error("Converse response had no output message")
    }
    return {
      request,
      result: {
        status: "ok",
        message,
        usage: response.usage ?? {
          inputTokens: 0,
          outputTokens: 0,
          totalTokens: 0,
        },
      },
    }
  } catch (error) {
    if (isWarmingUpError(error)) {
      return { request, result: { status: "warming-up" } }
    }
    throw error
  }
}

export function useConverse() {
  return useMutation({
    mutationFn: sendConverse,
  })
}
