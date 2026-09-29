import type {
  FoundationModelSummary,
  GuardrailSummary,
} from "@aws-sdk/client-bedrock"

import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"

const NO_GUARDRAIL = "__none__"

export interface PlaygroundControlsProps {
  models: FoundationModelSummary[]
  modelId: string
  onModelIdChange: (modelId: string) => void
  systemPrompt: string
  onSystemPromptChange: (value: string) => void
  temperature: number
  onTemperatureChange: (value: number) => void
  topP: number
  onTopPChange: (value: number) => void
  maxTokens: number
  onMaxTokensChange: (value: number) => void
  guardrails: GuardrailSummary[]
  guardrailId: string
  onGuardrailIdChange: (guardrailId: string) => void
  guardrailVersion: string
  onGuardrailVersionChange: (value: string) => void
}

// A field left blank by the user parses to NaN; the caller's last known value
// is kept rather than sending NaN into inferenceConfig.
function parseNumber(raw: string, fallback: number): number {
  const parsed = Number(raw)
  return Number.isNaN(parsed) ? fallback : parsed
}

export function PlaygroundControls({
  models,
  modelId,
  onModelIdChange,
  systemPrompt,
  onSystemPromptChange,
  temperature,
  onTemperatureChange,
  topP,
  onTopPChange,
  maxTokens,
  onMaxTokensChange,
  guardrails,
  guardrailId,
  onGuardrailIdChange,
  guardrailVersion,
  onGuardrailVersionChange,
}: PlaygroundControlsProps) {
  return (
    <div className="space-y-4">
      <Field>
        <FieldTitle>
          <label htmlFor="playground-model">Model</label>
        </FieldTitle>
        <Select
          onValueChange={(value) => {
            onModelIdChange(value ?? "")
          }}
          value={modelId}
        >
          <SelectTrigger className="w-full" id="playground-model">
            <SelectValue placeholder="Select a model" />
          </SelectTrigger>
          <SelectContent>
            {models.map((model) => (
              <SelectItem key={model.modelId} value={model.modelId ?? ""}>
                {model.modelName ?? model.modelId}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="playground-system-prompt">System prompt</label>
        </FieldTitle>
        <Textarea
          id="playground-system-prompt"
          onChange={(event) => {
            onSystemPromptChange(event.target.value)
          }}
          placeholder="Instructions or persona for the model"
          rows={3}
          value={systemPrompt}
        />
      </Field>

      <div className="grid grid-cols-3 gap-2">
        <Field>
          <FieldTitle>
            <label htmlFor="playground-temperature">Temperature</label>
          </FieldTitle>
          <Input
            id="playground-temperature"
            max={1}
            min={0}
            onChange={(event) => {
              onTemperatureChange(parseNumber(event.target.value, temperature))
            }}
            step={0.1}
            type="number"
            value={temperature}
          />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="playground-top-p">Top P</label>
          </FieldTitle>
          <Input
            id="playground-top-p"
            max={1}
            min={0}
            onChange={(event) => {
              onTopPChange(parseNumber(event.target.value, topP))
            }}
            step={0.1}
            type="number"
            value={topP}
          />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="playground-max-tokens">Max tokens</label>
          </FieldTitle>
          <Input
            id="playground-max-tokens"
            min={1}
            onChange={(event) => {
              onMaxTokensChange(parseNumber(event.target.value, maxTokens))
            }}
            step={1}
            type="number"
            value={maxTokens}
          />
        </Field>
      </div>

      <Field>
        <FieldTitle>
          <label htmlFor="playground-guardrail">Guardrail (optional)</label>
        </FieldTitle>
        <Select
          onValueChange={(value) => {
            onGuardrailIdChange(
              value === null || value === NO_GUARDRAIL ? "" : value,
            )
          }}
          value={guardrailId === "" ? NO_GUARDRAIL : guardrailId}
        >
          <SelectTrigger className="w-full" id="playground-guardrail">
            <SelectValue placeholder="None" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={NO_GUARDRAIL}>None</SelectItem>
            {guardrails.map((guardrail) => (
              <SelectItem key={guardrail.id} value={guardrail.id ?? ""}>
                {guardrail.name ?? guardrail.id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      {guardrailId !== "" && (
        <Field>
          <FieldTitle>
            <label htmlFor="playground-guardrail-version">
              Guardrail version
            </label>
          </FieldTitle>
          <Input
            id="playground-guardrail-version"
            onChange={(event) => {
              onGuardrailVersionChange(event.target.value)
            }}
            value={guardrailVersion}
          />
        </Field>
      )}
    </div>
  )
}
