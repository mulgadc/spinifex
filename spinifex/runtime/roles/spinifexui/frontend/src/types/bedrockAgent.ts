import type {
  CreateDataSourceCommandInput,
  CreateKnowledgeBaseCommandInput,
} from "@aws-sdk/client-bedrock-agent"
import { z } from "zod"

// Ochre only stores and echoes roleArn back — it never assumes the account's
// IAM actually grants it anything — so a stable placeholder satisfies the
// non-empty requirement when the field is left blank in the UI.
const DEFAULT_KNOWLEDGE_BASE_ROLE_ARN =
  "arn:aws:iam::local:role/ochre-knowledge-base"

// Ochre requires storageConfiguration to be non-nil but never inspects its
// contents. S3VectorsConfiguration is the only StorageConfiguration union
// member whose fields are all optional, so `{}` is the smallest shape the
// SDK will serialize without a client-side type error.
const SYNTHESIZED_STORAGE_CONFIGURATION: NonNullable<
  CreateKnowledgeBaseCommandInput["storageConfiguration"]
> = {
  type: "S3_VECTORS",
  s3VectorsConfiguration: {},
}

// Vector dimensions Ochre's self-hosted embedders are known to produce,
// keyed by the model id segment of the embedding model ARN (the part after
// `foundation-model/`). Anything else leaves the dimensions field blank for
// the user to fill in.
export const KNOWN_EMBEDDING_DIMENSIONS = {
  "nomic-embed-text-v1.5": 768,
  "bge-base-en-v1.5": 768,
} satisfies Record<string, number>

export function dimensionsForModelArn(
  embeddingModelArn: string,
): number | undefined {
  const [, modelId] = embeddingModelArn.split("foundation-model/")
  if (!modelId || !(modelId in KNOWN_EMBEDDING_DIMENSIONS)) {
    return undefined
  }
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- narrowed by the `in` check above
  const key = modelId as keyof typeof KNOWN_EMBEDDING_DIMENSIONS
  return KNOWN_EMBEDDING_DIMENSIONS[key]
}

const MIN_DIMENSIONS = 1

export const kbFormSchema = z.object({
  name: z.string().min(1, "Name is required"),
  description: z.string(),
  embeddingModelArn: z.string().min(1, "Embedding model is required"),
  dimensions: z
    .number()
    .int("Dimensions must be a whole number")
    .min(MIN_DIMENSIONS, "Dimensions must be greater than 0"),
  roleArn: z.string(),
})

export type KnowledgeBaseFormData = z.infer<typeof kbFormSchema>

export const EMPTY_KB_FORM_DEFAULTS: KnowledgeBaseFormData = {
  name: "",
  description: "",
  embeddingModelArn: "",
  dimensions: 0,
  roleArn: "",
}

// An optional string field is sent only when it carries something: an empty
// string is still a value as far as the SDK is concerned.
function optional(value: string): string | undefined {
  return value.length > 0 ? value : undefined
}

export function formToCreateKnowledgeBaseInput(
  data: KnowledgeBaseFormData,
): CreateKnowledgeBaseCommandInput {
  return {
    name: data.name,
    description: optional(data.description),
    roleArn:
      data.roleArn.length > 0 ? data.roleArn : DEFAULT_KNOWLEDGE_BASE_ROLE_ARN,
    knowledgeBaseConfiguration: {
      type: "VECTOR",
      vectorKnowledgeBaseConfiguration: {
        embeddingModelArn: data.embeddingModelArn,
        embeddingModelConfiguration: {
          bedrockEmbeddingModelConfiguration: {
            dimensions: data.dimensions,
          },
        },
      },
    },
    storageConfiguration: SYNTHESIZED_STORAGE_CONFIGURATION,
  }
}

// AWS's own S3 bucket ARN shape: no region or account segment.
const S3_BUCKET_ARN_PATTERN = /^arn:aws:s3:::\S+$/

const MIN_MAX_TOKENS = 1
const MIN_OVERLAP_PERCENTAGE = 0
const MAX_OVERLAP_PERCENTAGE = 99
const DEFAULT_MAX_TOKENS = 300
const DEFAULT_OVERLAP_PERCENTAGE = 20

export const dataSourceFormSchema = z.object({
  name: z.string().min(1, "Name is required"),
  bucketArn: z
    .string()
    .min(1, "Bucket ARN is required")
    .regex(S3_BUCKET_ARN_PATTERN, "Must be an S3 bucket ARN"),
  inclusionPrefix: z.string(),
  chunkingEnabled: z.boolean(),
  maxTokens: z
    .number()
    .int("Max tokens must be a whole number")
    .min(MIN_MAX_TOKENS, "Max tokens must be greater than 0"),
  overlapPercentage: z
    .number()
    .int("Overlap must be a whole number")
    .min(MIN_OVERLAP_PERCENTAGE, "Overlap must be between 0 and 99")
    .max(MAX_OVERLAP_PERCENTAGE, "Overlap must be between 0 and 99"),
})

export type DataSourceFormData = z.infer<typeof dataSourceFormSchema>

export const EMPTY_DATA_SOURCE_FORM_DEFAULTS: DataSourceFormData = {
  name: "",
  bucketArn: "",
  inclusionPrefix: "",
  chunkingEnabled: false,
  maxTokens: DEFAULT_MAX_TOKENS,
  overlapPercentage: DEFAULT_OVERLAP_PERCENTAGE,
}

export function formToCreateDataSourceInput(
  knowledgeBaseId: string,
  data: DataSourceFormData,
): CreateDataSourceCommandInput {
  return {
    knowledgeBaseId,
    name: data.name,
    dataSourceConfiguration: {
      type: "S3",
      s3Configuration: {
        bucketArn: data.bucketArn,
        inclusionPrefixes: optional(data.inclusionPrefix)
          ? [data.inclusionPrefix]
          : undefined,
      },
    },
    vectorIngestionConfiguration: data.chunkingEnabled
      ? {
          chunkingConfiguration: {
            chunkingStrategy: "FIXED_SIZE",
            fixedSizeChunkingConfiguration: {
              maxTokens: data.maxTokens,
              overlapPercentage: data.overlapPercentage,
            },
          },
        }
      : undefined,
  }
}
