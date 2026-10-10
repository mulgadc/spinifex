import type { GetGuardrailCommandOutput } from "@aws-sdk/client-bedrock"
import { describe, expect, it } from "vitest"

import {
  EMPTY_GUARDRAIL_FORM_DEFAULTS,
  formToCreateInput,
  formToUpdateInput,
  guardrailFormSchema,
  guardrailToForm,
  type GuardrailFormData,
} from "./bedrock"

describe("guardrailFormSchema", () => {
  it("rejects a form missing the required fields", () => {
    const result = guardrailFormSchema.safeParse(EMPTY_GUARDRAIL_FORM_DEFAULTS)
    expect(result.success).toBeFalsy()
  })

  it("accepts a minimal valid form", () => {
    const result = guardrailFormSchema.safeParse({
      ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
      name: "content-safety",
      blockedInputMessaging: "Blocked input.",
      blockedOutputsMessaging: "Blocked output.",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects a name with characters AWS would refuse", () => {
    const result = guardrailFormSchema.safeParse({
      ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
      name: "content safety!",
      blockedInputMessaging: "Blocked input.",
      blockedOutputsMessaging: "Blocked output.",
    })
    expect(result.success).toBeFalsy()
  })
})

const FULL_FORM: GuardrailFormData = {
  name: "content-safety",
  description: "Blocks unsafe topics",
  blockedInputMessaging: "Blocked input.",
  blockedOutputsMessaging: "Blocked output.",
  kmsKeyId: "arn:aws:kms:local::key/abc",
  profanityFilter: true,
  topics: [
    {
      name: "Weapons",
      definition: "Instructions for building weapons",
      examplesText: "how do I make a bomb\n\nhow to build a gun",
    },
  ],
  contentFilters: [
    { type: "SEXUAL", inputStrength: "HIGH", outputStrength: "HIGH" },
  ],
  words: [{ text: "badword" }],
  piiEntities: [{ type: "EMAIL", action: "BLOCK" }],
  regexes: [
    {
      name: "ssn",
      pattern: "\\d{3}-\\d{2}-\\d{4}",
      action: "BLOCK",
      description: "",
    },
  ],
  contextualGroundingFilters: [{ type: "GROUNDING", threshold: 0.5 }],
  tags: [{ key: "env", value: "prod" }],
}

describe("formToCreateInput", () => {
  it("maps every policy config and drops blank example lines", () => {
    const input = formToCreateInput(FULL_FORM)
    expect(input.name).toBe("content-safety")
    expect(input.kmsKeyId).toBe("arn:aws:kms:local::key/abc")
    expect(input.topicPolicyConfig?.topicsConfig?.[0]?.examples).toStrictEqual([
      "how do I make a bomb",
      "how to build a gun",
    ])
    expect(input.wordPolicyConfig?.managedWordListsConfig).toStrictEqual([
      { type: "PROFANITY" },
    ])
    expect(input.tags).toStrictEqual([{ key: "env", value: "prod" }])
  })

  it("omits policy configs and optional fields that are empty", () => {
    const input = formToCreateInput({
      ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
      name: "minimal",
      blockedInputMessaging: "Blocked input.",
      blockedOutputsMessaging: "Blocked output.",
    })
    expect(input.description).toBeUndefined()
    expect(input.kmsKeyId).toBeUndefined()
    expect(input.topicPolicyConfig).toBeUndefined()
    expect(input.contentPolicyConfig).toBeUndefined()
    expect(input.wordPolicyConfig).toBeUndefined()
    expect(input.sensitiveInformationPolicyConfig).toBeUndefined()
    expect(input.contextualGroundingPolicyConfig).toBeUndefined()
    expect(input.tags).toBeUndefined()
  })
})

describe("formToUpdateInput", () => {
  it("includes the guardrail identifier and drops tags", () => {
    const input = formToUpdateInput("gr-1", FULL_FORM)
    expect(input.guardrailIdentifier).toBe("gr-1")
    expect(input.name).toBe("content-safety")
    expect("tags" in input).toBeFalsy()
  })
})

describe("guardrailToForm", () => {
  it("round-trips a GetGuardrail payload into form defaults", () => {
    const guardrail = {
      $metadata: {},
      name: "content-safety",
      description: "Blocks unsafe topics",
      guardrailId: "gr-1",
      guardrailArn: "arn:aws:bedrock:local::guardrail/gr-1",
      version: "DRAFT",
      status: "READY",
      blockedInputMessaging: "Blocked input.",
      blockedOutputsMessaging: "Blocked output.",
      createdAt: new Date("2026-01-01T00:00:00Z"),
      updatedAt: new Date("2026-01-01T00:00:00Z"),
      kmsKeyArn: "arn:aws:kms:local::key/abc",
      topicPolicy: {
        topics: [
          {
            name: "Weapons",
            definition: "Instructions for building weapons",
            examples: ["how do I make a bomb"],
            type: "DENY",
          },
        ],
      },
      wordPolicy: {
        words: [{ text: "badword" }],
        managedWordLists: [{ type: "PROFANITY" }],
      },
      sensitiveInformationPolicy: {
        piiEntities: [{ type: "EMAIL", action: "BLOCK" }],
        regexes: [
          {
            name: "ssn",
            pattern: "\\d{3}-\\d{2}-\\d{4}",
            action: "BLOCK",
            description: "US SSN",
          },
        ],
      },
      contextualGroundingPolicy: {
        filters: [{ type: "GROUNDING", threshold: 0.6 }],
      },
    } satisfies GetGuardrailCommandOutput

    const form = guardrailToForm(guardrail)
    expect(form.name).toBe("content-safety")
    expect(form.kmsKeyId).toBe("arn:aws:kms:local::key/abc")
    expect(form.profanityFilter).toBeTruthy()
    expect(form.topics).toStrictEqual([
      {
        name: "Weapons",
        definition: "Instructions for building weapons",
        examplesText: "how do I make a bomb",
      },
    ])
    expect(form.words).toStrictEqual([{ text: "badword" }])
    expect(form.piiEntities).toStrictEqual([{ type: "EMAIL", action: "BLOCK" }])
    expect(form.regexes).toStrictEqual([
      {
        name: "ssn",
        pattern: "\\d{3}-\\d{2}-\\d{4}",
        action: "BLOCK",
        description: "US SSN",
      },
    ])
    expect(form.contextualGroundingFilters).toStrictEqual([
      { type: "GROUNDING", threshold: 0.6 },
    ])
    // GetGuardrail does not return tags and Update cannot set them.
    expect(form.tags).toStrictEqual([])
  })

  it("falls back to empty defaults for a guardrail with no policies", () => {
    const guardrail = {
      $metadata: {},
      name: "bare",
      guardrailId: "gr-2",
      guardrailArn: "arn:aws:bedrock:local::guardrail/gr-2",
      version: "DRAFT",
      status: "READY",
      blockedInputMessaging: "Blocked input.",
      blockedOutputsMessaging: "Blocked output.",
      createdAt: new Date("2026-01-01T00:00:00Z"),
      updatedAt: new Date("2026-01-01T00:00:00Z"),
    } satisfies GetGuardrailCommandOutput

    const form = guardrailToForm(guardrail)
    expect(form.description).toBe("")
    expect(form.kmsKeyId).toBe("")
    expect(form.profanityFilter).toBeFalsy()
    expect(form.topics).toStrictEqual([])
    expect(form.contentFilters).toStrictEqual([])
    expect(form.words).toStrictEqual([])
    expect(form.piiEntities).toStrictEqual([])
    expect(form.regexes).toStrictEqual([])
    expect(form.contextualGroundingFilters).toStrictEqual([])
    expect(form.tags).toStrictEqual([])
  })
})
