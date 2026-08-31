import { describe, expect, it } from "vitest"

import {
  dataSourceFormSchema,
  type DataSourceFormData,
  dimensionsForModelArn,
  EMPTY_DATA_SOURCE_FORM_DEFAULTS,
  EMPTY_KB_FORM_DEFAULTS,
  formToCreateDataSourceInput,
  formToCreateKnowledgeBaseInput,
  kbFormSchema,
  type KnowledgeBaseFormData,
} from "./bedrockAgent"

describe("kbFormSchema", () => {
  it("rejects a form missing the required fields", () => {
    const result = kbFormSchema.safeParse(EMPTY_KB_FORM_DEFAULTS)
    expect(result.success).toBeFalsy()
  })

  it("rejects dimensions that are zero or negative", () => {
    const result = kbFormSchema.safeParse({
      ...EMPTY_KB_FORM_DEFAULTS,
      name: "docs-kb",
      embeddingModelArn:
        "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
      dimensions: 0,
    })
    expect(result.success).toBeFalsy()
  })

  it("accepts a minimal valid form", () => {
    const result = kbFormSchema.safeParse({
      ...EMPTY_KB_FORM_DEFAULTS,
      name: "docs-kb",
      embeddingModelArn:
        "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
      dimensions: 768,
    })
    expect(result.success).toBeTruthy()
  })
})

describe("dimensionsForModelArn", () => {
  it("returns the known dimensions for a self-hosted embedder", () => {
    expect(
      dimensionsForModelArn(
        "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
      ),
    ).toBe(768)
  })

  it("returns undefined for an unknown model", () => {
    expect(
      dimensionsForModelArn(
        "arn:aws:bedrock:local::foundation-model/some-unknown-model",
      ),
    ).toBeUndefined()
  })
})

const FULL_KB_FORM: KnowledgeBaseFormData = {
  name: "docs-kb",
  description: "Product docs",
  embeddingModelArn:
    "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
  dimensions: 768,
  roleArn: "",
}

describe("formToCreateKnowledgeBaseInput", () => {
  it("synthesizes a non-nil storageConfiguration", () => {
    const input = formToCreateKnowledgeBaseInput(FULL_KB_FORM)
    expect(input.storageConfiguration).toBeDefined()
    expect(input.storageConfiguration?.type).toBe("S3_VECTORS")
  })

  it("builds a VECTOR knowledgeBaseConfiguration with the embedding model and dimensions", () => {
    const input = formToCreateKnowledgeBaseInput(FULL_KB_FORM)
    expect(input.knowledgeBaseConfiguration?.type).toBe("VECTOR")
    const vectorConfig =
      input.knowledgeBaseConfiguration?.vectorKnowledgeBaseConfiguration
    expect(vectorConfig?.embeddingModelArn).toBe(FULL_KB_FORM.embeddingModelArn)
    expect(
      vectorConfig?.embeddingModelConfiguration
        ?.bedrockEmbeddingModelConfiguration?.dimensions,
    ).toBe(768)
  })

  it("defaults roleArn when the field is blank", () => {
    const input = formToCreateKnowledgeBaseInput(FULL_KB_FORM)
    expect(input.roleArn).toBeTruthy()
    expect(input.roleArn?.length).toBeGreaterThan(0)
  })

  it("uses the given roleArn when provided", () => {
    const input = formToCreateKnowledgeBaseInput({
      ...FULL_KB_FORM,
      roleArn: "arn:aws:iam::local:role/custom",
    })
    expect(input.roleArn).toBe("arn:aws:iam::local:role/custom")
  })

  it("omits description when blank", () => {
    const input = formToCreateKnowledgeBaseInput({
      ...FULL_KB_FORM,
      description: "",
    })
    expect(input.description).toBeUndefined()
  })
})

describe("dataSourceFormSchema", () => {
  it("rejects a form missing name and bucketArn", () => {
    const result = dataSourceFormSchema.safeParse(
      EMPTY_DATA_SOURCE_FORM_DEFAULTS,
    )
    expect(result.success).toBeFalsy()
  })

  it("rejects a bucketArn that is not an S3 ARN", () => {
    const result = dataSourceFormSchema.safeParse({
      ...EMPTY_DATA_SOURCE_FORM_DEFAULTS,
      name: "s3-docs",
      bucketArn: "not-an-arn",
    })
    expect(result.success).toBeFalsy()
  })

  it("accepts a minimal valid form", () => {
    const result = dataSourceFormSchema.safeParse({
      ...EMPTY_DATA_SOURCE_FORM_DEFAULTS,
      name: "s3-docs",
      bucketArn: "arn:aws:s3:::docs-bucket",
    })
    expect(result.success).toBeTruthy()
  })
})

const FULL_DATA_SOURCE_FORM: DataSourceFormData = {
  ...EMPTY_DATA_SOURCE_FORM_DEFAULTS,
  name: "s3-docs",
  bucketArn: "arn:aws:s3:::docs-bucket",
}

describe("formToCreateDataSourceInput", () => {
  it("builds the S3 dataSourceConfiguration with the knowledge base id", () => {
    const input = formToCreateDataSourceInput("kb-1", FULL_DATA_SOURCE_FORM)
    expect(input.knowledgeBaseId).toBe("kb-1")
    expect(input.dataSourceConfiguration?.type).toBe("S3")
    expect(input.dataSourceConfiguration?.s3Configuration?.bucketArn).toBe(
      "arn:aws:s3:::docs-bucket",
    )
  })

  it("omits inclusionPrefixes when the prefix is blank", () => {
    const input = formToCreateDataSourceInput("kb-1", FULL_DATA_SOURCE_FORM)
    expect(
      input.dataSourceConfiguration?.s3Configuration?.inclusionPrefixes,
    ).toBeUndefined()
  })

  it("sets inclusionPrefixes to a single-element array when provided", () => {
    const input = formToCreateDataSourceInput("kb-1", {
      ...FULL_DATA_SOURCE_FORM,
      inclusionPrefix: "docs/",
    })
    expect(
      input.dataSourceConfiguration?.s3Configuration?.inclusionPrefixes,
    ).toStrictEqual(["docs/"])
  })

  it("omits vectorIngestionConfiguration when chunking is disabled", () => {
    const input = formToCreateDataSourceInput("kb-1", FULL_DATA_SOURCE_FORM)
    expect(input.vectorIngestionConfiguration).toBeUndefined()
  })

  it("builds a FIXED_SIZE chunkingConfiguration when chunking is enabled", () => {
    const input = formToCreateDataSourceInput("kb-1", {
      ...FULL_DATA_SOURCE_FORM,
      chunkingEnabled: true,
      maxTokens: 400,
      overlapPercentage: 15,
    })
    const chunking = input.vectorIngestionConfiguration?.chunkingConfiguration
    expect(chunking?.chunkingStrategy).toBe("FIXED_SIZE")
    expect(chunking?.fixedSizeChunkingConfiguration).toStrictEqual({
      maxTokens: 400,
      overlapPercentage: 15,
    })
  })
})
