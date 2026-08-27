import { screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import {
  adminOchreCatalogQueryOptions,
  type AdminCatalogEntry,
} from "@/queries/admin"
import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

import { ModelCatalogPage } from "./model-catalog-page"

const SELF_HOST_ENTRY = {
  modelId: "meta.llama3-2-1b-instruct-v1:0",
  modelName: "Llama 3.2 1B Instruct",
  family: "vllm",
  inputModalities: ["TEXT"],
  outputModalities: ["TEXT"],
  responseStreamingSupported: false,
  inputPriceMicroUsdPerMillion: 0,
  outputPriceMicroUsdPerMillion: 0,
  priceKnown: false,
  minVramMib: 5120,
  instanceType: "g5.xlarge",
  coServeGroup: "ochre-demo-bundle",
  availability: "available",
} satisfies AdminCatalogEntry

const EMBED_ENTRY = {
  ...SELF_HOST_ENTRY,
  modelId: "nomic-embed-text-v1.5",
  modelName: "Nomic Embed Text v1.5",
  family: "tei",
  inputModalities: ["TEXT"],
  outputModalities: ["EMBEDDING"],
  coServeGroup: "",
  availability: "ungranted",
} satisfies AdminCatalogEntry

function seed(entries: AdminCatalogEntry[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(adminOchreCatalogQueryOptions.queryKey, { entries })
  return qc
}

describe("ModelCatalogPage", () => {
  it("shows the empty state when there are no models", () => {
    renderWithClient(<ModelCatalogPage />, seed([]))
    expect(screen.getByText("No models found.")).toBeInTheDocument()
  })

  it("renders model rows sorted by name", () => {
    renderWithClient(
      <ModelCatalogPage />,
      seed([
        { ...SELF_HOST_ENTRY, modelName: "Zebra Model", modelId: "zebra" },
        { ...SELF_HOST_ENTRY, modelName: "Alpha Model", modelId: "alpha" },
      ]),
    )
    const names = screen.getAllByText(/ Model$/).map((el) => el.textContent)
    expect(names).toStrictEqual(["Alpha Model", "Zebra Model"])
  })

  it("renders a modality badge for each unique input/output modality", () => {
    renderWithClient(<ModelCatalogPage />, seed([EMBED_ENTRY]))
    expect(screen.getByText("TEXT")).toBeInTheDocument()
    expect(screen.getByText("EMBEDDING")).toBeInTheDocument()
  })

  it("shows self-host models as included with VRAM and instance type", () => {
    renderWithClient(<ModelCatalogPage />, seed([SELF_HOST_ENTRY]))
    expect(screen.getByText("Included")).toBeInTheDocument()
    expect(screen.getByText("5 GiB · g5.xlarge")).toBeInTheDocument()
  })

  it("shows the family as an operator ops detail", () => {
    renderWithClient(<ModelCatalogPage />, seed([SELF_HOST_ENTRY]))
    expect(screen.getByText("vllm")).toBeInTheDocument()
  })

  it("shows a streaming badge only when supported", () => {
    renderWithClient(
      <ModelCatalogPage />,
      seed([{ ...SELF_HOST_ENTRY, responseStreamingSupported: true }]),
    )
    expect(
      screen.getByText("Streaming", { selector: '[data-slot="badge"]' }),
    ).toBeInTheDocument()
  })

  it("renders the available badge with no fix affordance", () => {
    renderWithClient(<ModelCatalogPage />, seed([SELF_HOST_ENTRY]))
    expect(screen.getByText("Available")).toBeInTheDocument()
  })

  it("renders the ungranted reason with a grant-access affordance", () => {
    renderWithClient(<ModelCatalogPage />, seed([EMBED_ENTRY]))
    expect(screen.getByText("Ungranted")).toBeInTheDocument()
    expect(
      screen.getByText("Grant access to enable this model"),
    ).toBeInTheDocument()
  })

  it("renders the no-weights-staged reason with a stage-weights affordance", () => {
    renderWithClient(
      <ModelCatalogPage />,
      seed([{ ...SELF_HOST_ENTRY, availability: "no-weights-staged" }]),
    )
    expect(screen.getByText("No weights staged")).toBeInTheDocument()
    expect(
      screen.getByText("Stage weights to enable this model"),
    ).toBeInTheDocument()
  })

  it("renders the no-credential reason with an add-credential affordance", () => {
    renderWithClient(
      <ModelCatalogPage />,
      seed([{ ...SELF_HOST_ENTRY, availability: "no-credential" }]),
    )
    expect(screen.getByText("No credential")).toBeInTheDocument()
    expect(
      screen.getByText("Add a provider credential to enable this model"),
    ).toBeInTheDocument()
  })

  it("shows the co-serve group when present, dash otherwise", () => {
    renderWithClient(<ModelCatalogPage />, seed([SELF_HOST_ENTRY, EMBED_ENTRY]))
    expect(screen.getByText("ochre-demo-bundle")).toBeInTheDocument()
  })
})
