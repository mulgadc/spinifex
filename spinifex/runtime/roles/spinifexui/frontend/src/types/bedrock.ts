import type {
  CreateGuardrailCommandInput,
  GetGuardrailCommandOutput,
  GuardrailContentFilterType,
  GuardrailContextualGroundingFilterType,
  GuardrailFilterStrength,
  GuardrailPiiEntityType,
  GuardrailSensitiveInformationAction,
  UpdateGuardrailCommandInput,
} from "@aws-sdk/client-bedrock"
import { z } from "zod"

// Mirrors the field constraints CreateGuardrail/UpdateGuardrail enforce, so the
// form rejects what the backend would reject before the round trip. Value
// lists are pinned to the SDK's own enums via `satisfies` so a mismatch with a
// future SDK bump is a compile error rather than a silently stale dropdown.

export const CONTENT_FILTER_TYPES = [
  "SEXUAL",
  "VIOLENCE",
  "HATE",
  "INSULTS",
  "MISCONDUCT",
  "PROMPT_ATTACK",
] as const satisfies readonly GuardrailContentFilterType[]

export const FILTER_STRENGTHS = [
  "NONE",
  "LOW",
  "MEDIUM",
  "HIGH",
] as const satisfies readonly GuardrailFilterStrength[]

export const CONTEXTUAL_GROUNDING_FILTER_TYPES = [
  "GROUNDING",
  "RELEVANCE",
] as const satisfies readonly GuardrailContextualGroundingFilterType[]

export const SENSITIVE_INFORMATION_ACTIONS = [
  "BLOCK",
  "ANONYMIZE",
  "NONE",
] as const satisfies readonly GuardrailSensitiveInformationAction[]

export const PII_ENTITY_TYPES = [
  "ADDRESS",
  "AGE",
  "AWS_ACCESS_KEY",
  "AWS_SECRET_KEY",
  "CA_HEALTH_NUMBER",
  "CA_SOCIAL_INSURANCE_NUMBER",
  "CREDIT_DEBIT_CARD_CVV",
  "CREDIT_DEBIT_CARD_EXPIRY",
  "CREDIT_DEBIT_CARD_NUMBER",
  "DRIVER_ID",
  "EMAIL",
  "INTERNATIONAL_BANK_ACCOUNT_NUMBER",
  "IP_ADDRESS",
  "LICENSE_PLATE",
  "MAC_ADDRESS",
  "NAME",
  "PASSWORD",
  "PHONE",
  "PIN",
  "SWIFT_CODE",
  "UK_NATIONAL_HEALTH_SERVICE_NUMBER",
  "UK_NATIONAL_INSURANCE_NUMBER",
  "UK_UNIQUE_TAXPAYER_REFERENCE_NUMBER",
  "URL",
  "USERNAME",
  "US_BANK_ACCOUNT_NUMBER",
  "US_BANK_ROUTING_NUMBER",
  "US_INDIVIDUAL_TAX_IDENTIFICATION_NUMBER",
  "US_PASSPORT_NUMBER",
  "US_SOCIAL_SECURITY_NUMBER",
  "VEHICLE_IDENTIFICATION_NUMBER",
] as const satisfies readonly GuardrailPiiEntityType[]

// The only managed word list Bedrock currently offers, so it is exposed as a
// single toggle rather than a picker over a list of one.
const MANAGED_WORD_LIST_PROFANITY = "PROFANITY"

const MAX_GUARDRAIL_NAME_LEN = 50

// AWS's own guardrail name constraint: letters, digits, hyphens and
// underscores. Looser rules here would let the form accept a name Create
// would refuse.
export const guardrailNameField = z
  .string()
  .min(1, "Name is required")
  .max(
    MAX_GUARDRAIL_NAME_LEN,
    `Name must be at most ${MAX_GUARDRAIL_NAME_LEN} characters`,
  )
  .regex(
    /^[0-9a-zA-Z-_]+$/,
    "Name may contain only letters, digits, hyphens and underscores",
  )

export const guardrailTopicSchema = z.object({
  name: z.string().min(1, "Topic name is required"),
  definition: z.string().min(1, "Definition is required"),
  // One example prompt per line; blank lines are dropped on submit.
  examplesText: z.string(),
})

export type GuardrailTopicFormData = z.infer<typeof guardrailTopicSchema>

export const guardrailContentFilterSchema = z.object({
  type: z.enum(CONTENT_FILTER_TYPES),
  inputStrength: z.enum(FILTER_STRENGTHS),
  outputStrength: z.enum(FILTER_STRENGTHS),
})

export const guardrailWordSchema = z.object({
  text: z.string().min(1, "Word text is required"),
})

export const guardrailPiiEntitySchema = z.object({
  type: z.enum(PII_ENTITY_TYPES),
  action: z.enum(SENSITIVE_INFORMATION_ACTIONS),
})

export const guardrailRegexSchema = z.object({
  name: z.string().min(1, "Name is required"),
  pattern: z.string().min(1, "Pattern is required"),
  action: z.enum(SENSITIVE_INFORMATION_ACTIONS),
  description: z.string(),
})

const MIN_THRESHOLD = 0
const MAX_THRESHOLD = 1

export const guardrailContextualGroundingFilterSchema = z.object({
  type: z.enum(CONTEXTUAL_GROUNDING_FILTER_TYPES),
  threshold: z
    .number()
    .min(MIN_THRESHOLD, "Threshold must be between 0 and 1")
    .max(MAX_THRESHOLD, "Threshold must be between 0 and 1"),
})

export const guardrailTagSchema = z.object({
  key: z.string().min(1, "Tag key is required").max(128),
  value: z.string().max(256),
})

export const guardrailFormSchema = z.object({
  name: guardrailNameField,
  description: z.string(),
  blockedInputMessaging: z.string().min(1, "Blocked input message is required"),
  blockedOutputsMessaging: z
    .string()
    .min(1, "Blocked output message is required"),
  kmsKeyId: z.string(),
  profanityFilter: z.boolean(),
  topics: z.array(guardrailTopicSchema),
  contentFilters: z.array(guardrailContentFilterSchema),
  words: z.array(guardrailWordSchema),
  piiEntities: z.array(guardrailPiiEntitySchema),
  regexes: z.array(guardrailRegexSchema),
  contextualGroundingFilters: z.array(guardrailContextualGroundingFilterSchema),
  tags: z.array(guardrailTagSchema),
})

export type GuardrailFormData = z.infer<typeof guardrailFormSchema>

export const EMPTY_GUARDRAIL_FORM_DEFAULTS: GuardrailFormData = {
  name: "",
  description: "",
  blockedInputMessaging: "",
  blockedOutputsMessaging: "",
  kmsKeyId: "",
  profanityFilter: false,
  topics: [],
  contentFilters: [],
  words: [],
  piiEntities: [],
  regexes: [],
  contextualGroundingFilters: [],
  tags: [],
}

// An optional string field is sent only when it carries something: an empty
// string is still a value as far as the SDK is concerned.
function optional(value: string): string | undefined {
  return value.length > 0 ? value : undefined
}

function examplesFromText(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.length > 0)
}

function toTopicPolicyConfig(
  topics: GuardrailTopicFormData[],
): CreateGuardrailCommandInput["topicPolicyConfig"] {
  if (topics.length === 0) {
    return undefined
  }
  return {
    topicsConfig: topics.map((topic) => ({
      name: topic.name,
      definition: topic.definition,
      type: "DENY",
      examples: examplesFromText(topic.examplesText),
    })),
  }
}

function toContentPolicyConfig(
  filters: GuardrailFormData["contentFilters"],
): CreateGuardrailCommandInput["contentPolicyConfig"] {
  if (filters.length === 0) {
    return undefined
  }
  return { filtersConfig: filters }
}

function toWordPolicyConfig(
  words: GuardrailFormData["words"],
  profanityFilter: boolean,
): CreateGuardrailCommandInput["wordPolicyConfig"] {
  if (words.length === 0 && !profanityFilter) {
    return undefined
  }
  return {
    wordsConfig: words.length > 0 ? words : undefined,
    managedWordListsConfig: profanityFilter
      ? [{ type: MANAGED_WORD_LIST_PROFANITY }]
      : undefined,
  }
}

function toSensitiveInformationPolicyConfig(
  piiEntities: GuardrailFormData["piiEntities"],
  regexes: GuardrailFormData["regexes"],
): CreateGuardrailCommandInput["sensitiveInformationPolicyConfig"] {
  if (piiEntities.length === 0 && regexes.length === 0) {
    return undefined
  }
  return {
    piiEntitiesConfig: piiEntities.length > 0 ? piiEntities : undefined,
    regexesConfig:
      regexes.length > 0
        ? regexes.map((regex) => ({
            name: regex.name,
            pattern: regex.pattern,
            action: regex.action,
            description: optional(regex.description),
          }))
        : undefined,
  }
}

function toContextualGroundingPolicyConfig(
  filters: GuardrailFormData["contextualGroundingFilters"],
): CreateGuardrailCommandInput["contextualGroundingPolicyConfig"] {
  if (filters.length === 0) {
    return undefined
  }
  return { filtersConfig: filters }
}

function toGuardrailTags(
  tags: GuardrailFormData["tags"],
): CreateGuardrailCommandInput["tags"] {
  const set = tags
    .filter((tag) => tag.key.length > 0)
    .map((tag) => ({ key: tag.key, value: tag.value }))
  return set.length > 0 ? set : undefined
}

export function formToCreateInput(
  data: GuardrailFormData,
): CreateGuardrailCommandInput {
  return {
    name: data.name,
    description: optional(data.description),
    blockedInputMessaging: data.blockedInputMessaging,
    blockedOutputsMessaging: data.blockedOutputsMessaging,
    kmsKeyId: optional(data.kmsKeyId),
    topicPolicyConfig: toTopicPolicyConfig(data.topics),
    contentPolicyConfig: toContentPolicyConfig(data.contentFilters),
    wordPolicyConfig: toWordPolicyConfig(data.words, data.profanityFilter),
    sensitiveInformationPolicyConfig: toSensitiveInformationPolicyConfig(
      data.piiEntities,
      data.regexes,
    ),
    contextualGroundingPolicyConfig: toContextualGroundingPolicyConfig(
      data.contextualGroundingFilters,
    ),
    tags: toGuardrailTags(data.tags),
  }
}

// UpdateGuardrail mutates the DRAFT version only and has no tags or
// clientRequestToken of its own.
export function formToUpdateInput(
  guardrailIdentifier: string,
  data: GuardrailFormData,
): UpdateGuardrailCommandInput {
  return {
    guardrailIdentifier,
    name: data.name,
    description: optional(data.description),
    blockedInputMessaging: data.blockedInputMessaging,
    blockedOutputsMessaging: data.blockedOutputsMessaging,
    kmsKeyId: optional(data.kmsKeyId),
    topicPolicyConfig: toTopicPolicyConfig(data.topics),
    contentPolicyConfig: toContentPolicyConfig(data.contentFilters),
    wordPolicyConfig: toWordPolicyConfig(data.words, data.profanityFilter),
    sensitiveInformationPolicyConfig: toSensitiveInformationPolicyConfig(
      data.piiEntities,
      data.regexes,
    ),
    contextualGroundingPolicyConfig: toContextualGroundingPolicyConfig(
      data.contextualGroundingFilters,
    ),
  }
}

// Maps GetGuardrail's read shapes back into the *Config write shapes the edit
// form submits. The two are structurally close but distinct SDK types, so
// this is a field-by-field translation rather than a cast.
export function guardrailToForm(
  guardrail: GetGuardrailCommandOutput,
): GuardrailFormData {
  const topics = guardrail.topicPolicy?.topics ?? []
  const contentFilters = guardrail.contentPolicy?.filters ?? []
  const words = guardrail.wordPolicy?.words ?? []
  const managedWordLists = guardrail.wordPolicy?.managedWordLists ?? []
  const piiEntities = guardrail.sensitiveInformationPolicy?.piiEntities ?? []
  const regexes = guardrail.sensitiveInformationPolicy?.regexes ?? []
  const contextualGroundingFilters =
    guardrail.contextualGroundingPolicy?.filters ?? []

  return {
    name: guardrail.name ?? "",
    description: guardrail.description ?? "",
    blockedInputMessaging: guardrail.blockedInputMessaging ?? "",
    blockedOutputsMessaging: guardrail.blockedOutputsMessaging ?? "",
    // GetGuardrail returns the KMS key as an ARN (kmsKeyArn); Create/Update
    // accept an ARN in kmsKeyId too, so the value round-trips unchanged.
    kmsKeyId: guardrail.kmsKeyArn ?? "",
    profanityFilter: managedWordLists.some(
      (list) => list.type === MANAGED_WORD_LIST_PROFANITY,
    ),
    topics: topics.map((topic) => ({
      name: topic.name ?? "",
      definition: topic.definition ?? "",
      examplesText: (topic.examples ?? []).join("\n"),
    })),
    contentFilters: contentFilters.map((filter) => ({
      type: filter.type ?? "SEXUAL",
      inputStrength: filter.inputStrength ?? "NONE",
      outputStrength: filter.outputStrength ?? "NONE",
    })),
    words: words.map((word) => ({ text: word.text ?? "" })),
    piiEntities: piiEntities.map((entity) => ({
      type: entity.type ?? "EMAIL",
      action: entity.action ?? "BLOCK",
    })),
    regexes: regexes.map((regex) => ({
      name: regex.name ?? "",
      pattern: regex.pattern ?? "",
      action: regex.action ?? "BLOCK",
      description: regex.description ?? "",
    })),
    contextualGroundingFilters: contextualGroundingFilters.map((filter) => ({
      type: filter.type ?? "GROUNDING",
      threshold: filter.threshold ?? 0,
    })),
    // GetGuardrail does not return tags, and UpdateGuardrail cannot change
    // them, so the edit form never populates or submits this field.
    tags: [],
  }
}
