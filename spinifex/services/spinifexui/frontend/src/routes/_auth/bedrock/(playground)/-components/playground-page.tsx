import type {
  ConverseCommandInput,
  Message,
} from "@aws-sdk/client-bedrock-runtime"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type CliCommand,
  CliCommandPanel,
} from "@/components/cli-command-panel"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import {
  buildConverseInput,
  type ConverseOutcome,
  useConverse,
} from "@/mutations/converse"
import {
  foundationModelsQueryOptions,
  guardrailsQueryOptions,
} from "@/queries/bedrock"

import { ConversationPane } from "./conversation-pane"
import { PlaygroundControls } from "./playground-controls"
import { RawJsonPanel } from "./raw-json-panel"
import type { Turn } from "./types"

const DEFAULT_TEMPERATURE = 0.7
const DEFAULT_TOP_P = 0.9
const DEFAULT_MAX_TOKENS = 512

function nextId(): string {
  return crypto.randomUUID()
}

function toMessage(turn: Turn): Message {
  return { role: turn.role, content: [{ text: turn.text }] }
}

function extractText(message: Message | undefined): string {
  return (message?.content ?? []).map((block) => block.text ?? "").join("")
}

// Mirrors ConverseCommand: --model-id, --messages, --system,
// --inference-config and --guardrail-config are the fields buildConverseInput
// ever sets, so this stays in lockstep with the transport module by
// construction rather than by hand-kept parity.
function buildConverseCommands(request: ConverseCommandInput): CliCommand[] {
  const parts: CliCommand["parts"] = [
    { type: "bin", value: "aws bedrock-runtime converse" },
    { type: "flag", value: " \\\n  --model-id" },
    { type: "value", value: ` ${request.modelId}` },
    { type: "flag", value: " \\\n  --messages" },
    { type: "value", value: ` '${JSON.stringify(request.messages ?? [])}'` },
  ]

  if (request.system) {
    parts.push(
      { type: "flag", value: " \\\n  --system" },
      { type: "value", value: ` '${JSON.stringify(request.system)}'` },
    )
  }
  if (request.inferenceConfig) {
    parts.push(
      { type: "flag", value: " \\\n  --inference-config" },
      { type: "value", value: ` '${JSON.stringify(request.inferenceConfig)}'` },
    )
  }
  if (request.guardrailConfig) {
    parts.push(
      { type: "flag", value: " \\\n  --guardrail-config" },
      { type: "value", value: ` '${JSON.stringify(request.guardrailConfig)}'` },
    )
  }

  return [{ label: "Converse", parts }]
}

export function PlaygroundPage() {
  const { data: modelsData } = useSuspenseQuery(foundationModelsQueryOptions)
  const { data: guardrailsData } = useSuspenseQuery(guardrailsQueryOptions)
  const models = modelsData.modelSummaries ?? []
  const guardrails = guardrailsData.guardrails ?? []

  const [modelId, setModelId] = useState(models[0]?.modelId ?? "")
  const [systemPrompt, setSystemPrompt] = useState("")
  const [temperature, setTemperature] = useState(DEFAULT_TEMPERATURE)
  const [topP, setTopP] = useState(DEFAULT_TOP_P)
  const [maxTokens, setMaxTokens] = useState(DEFAULT_MAX_TOKENS)
  const [guardrailId, setGuardrailId] = useState("")
  const [guardrailVersion, setGuardrailVersion] = useState("DRAFT")
  const [inputText, setInputText] = useState("")
  const [turns, setTurns] = useState<Turn[]>([])
  const [lastOutcome, setLastOutcome] = useState<ConverseOutcome | null>(null)

  const converse = useConverse()

  function patchTurn(turnId: string, patch: Partial<Turn>) {
    setTurns((prev) =>
      prev.map((turn) => (turn.id === turnId ? { ...turn, ...patch } : turn)),
    )
  }

  async function runConverse(history: Turn[], assistantTurnId: string) {
    try {
      const outcome = await converse.mutateAsync({
        modelId,
        messages: history.map(toMessage),
        system: systemPrompt,
        inferenceConfig: { temperature, topP, maxTokens },
        guardrailIdentifier: guardrailId,
        guardrailVersion: guardrailId === "" ? undefined : guardrailVersion,
      })
      setLastOutcome(outcome)
      if (outcome.result.status === "ok") {
        patchTurn(assistantTurnId, {
          text: extractText(outcome.result.message),
          status: "complete",
          usage: outcome.result.usage,
        })
        return true
      }
      patchTurn(assistantTurnId, { status: "warming-up" })
      return false
    } catch (error) {
      patchTurn(assistantTurnId, {
        status: "error",
        errorMessage:
          error instanceof Error ? error.message : "Converse request failed",
      })
      return false
    }
  }

  async function handleSend() {
    const text = inputText
    if (text.trim().length === 0 || converse.isPending) {
      return
    }

    const userTurn: Turn = {
      id: nextId(),
      role: "user",
      text,
      status: "complete",
    }
    const assistantTurn: Turn = {
      id: nextId(),
      role: "assistant",
      text: "",
      status: "pending",
    }
    const history = [...turns, userTurn]
    setTurns([...history, assistantTurn])
    setInputText("")

    const ok = await runConverse(history, assistantTurn.id)
    if (!ok) {
      // The turn stayed in the pane as a warming-up/error state with its own
      // Retry action, but the compose box gets the text back so nothing typed
      // is lost.
      setInputText(text)
    }
  }

  async function handleRetry(turnId: string) {
    const index = turns.findIndex((turn) => turn.id === turnId)
    if (index === -1) {
      return
    }
    const history = turns.slice(0, index)
    patchTurn(turnId, { status: "pending", errorMessage: undefined })
    await runConverse(history, turnId)
  }

  const previewMessages = turns.filter((turn) => turn.status === "complete")
  const previewRequest = buildConverseInput({
    modelId,
    messages: previewMessages.map(toMessage),
    system: systemPrompt,
    inferenceConfig: { temperature, topP, maxTokens },
    guardrailIdentifier: guardrailId,
    guardrailVersion: guardrailId === "" ? undefined : guardrailVersion,
  })

  return (
    <>
      <PageHeading title="Playground" />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="space-y-4">
          <ConversationPane onRetry={handleRetry} turns={turns} />

          <form
            className="flex gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              void handleSend()
            }}
          >
            <Textarea
              onChange={(event) => {
                setInputText(event.target.value)
              }}
              placeholder="Send a message"
              rows={2}
              value={inputText}
            />
            <Button
              className="self-end"
              disabled={
                inputText.trim().length === 0 ||
                converse.isPending ||
                modelId === ""
              }
              type="submit"
            >
              {converse.isPending ? "Sending…" : "Send"}
            </Button>
          </form>

          <RawJsonPanel
            request={lastOutcome?.request}
            response={lastOutcome?.result}
          />

          <CliCommandPanel commands={buildConverseCommands(previewRequest)} />
        </div>

        <PlaygroundControls
          guardrailId={guardrailId}
          guardrails={guardrails}
          guardrailVersion={guardrailVersion}
          maxTokens={maxTokens}
          modelId={modelId}
          models={models}
          onGuardrailIdChange={setGuardrailId}
          onGuardrailVersionChange={setGuardrailVersion}
          onMaxTokensChange={setMaxTokens}
          onModelIdChange={setModelId}
          onSystemPromptChange={setSystemPrompt}
          onTemperatureChange={setTemperature}
          onTopPChange={setTopP}
          systemPrompt={systemPrompt}
          temperature={temperature}
          topP={topP}
        />
      </div>
    </>
  )
}
