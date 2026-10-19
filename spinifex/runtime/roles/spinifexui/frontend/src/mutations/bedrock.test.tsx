import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { guardrailsQueryOptions } from "@/queries/bedrock"
import {
  EMPTY_GUARDRAIL_FORM_DEFAULTS,
  type GuardrailFormData,
} from "@/types/bedrock"

import {
  useCreateGuardrail,
  useCreateGuardrailVersion,
  useDeleteGuardrail,
  useUpdateGuardrail,
} from "./bedrock"

const FORM: GuardrailFormData = {
  ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
  name: "content-safety",
  blockedInputMessaging: "Blocked input.",
  blockedOutputsMessaging: "Blocked output.",
}

describe("useCreateGuardrail", () => {
  it("sends CreateGuardrailCommand with the form values", async () => {
    const { result } = renderMutationHook(() => useCreateGuardrail())

    result.current.mutate(FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("content-safety")
    expect(input.blockedInputMessaging).toBe("Blocked input.")
    expect(input.blockedOutputsMessaging).toBe("Blocked output.")
  })

  it("marks the cached guardrail list stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(guardrailsQueryOptions.queryKey, {
      guardrails: [],
      $metadata: {},
    })

    const { result } = renderMutationHook(
      () => useCreateGuardrail(),
      queryClient,
    )

    result.current.mutate(FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(guardrailsQueryOptions.queryKey)?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useUpdateGuardrail", () => {
  it("sends UpdateGuardrailCommand with the identifier and form values", async () => {
    const { result } = renderMutationHook(() => useUpdateGuardrail())

    result.current.mutate({ guardrailIdentifier: "gr-1", data: FORM })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.guardrailIdentifier).toBe("gr-1")
    expect(input.name).toBe("content-safety")
  })

  it("invalidates the list and the single guardrail on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useUpdateGuardrail(),
      queryClient,
    )

    result.current.mutate({ guardrailIdentifier: "gr-1", data: FORM })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["bedrock", "guardrails"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock", "guardrails", "gr-1"],
    })
  })
})

describe("useDeleteGuardrail", () => {
  it("sends DeleteGuardrailCommand with the identifier", async () => {
    const { result } = renderMutationHook(() => useDeleteGuardrail())

    result.current.mutate("gr-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
    })
  })

  it("invalidates the list and the single guardrail on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeleteGuardrail(),
      queryClient,
    )

    result.current.mutate("gr-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["bedrock", "guardrails"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock", "guardrails", "gr-1"],
    })
  })
})

describe("useCreateGuardrailVersion", () => {
  it("sends CreateGuardrailVersionCommand with the identifier and description", async () => {
    const { result } = renderMutationHook(() => useCreateGuardrailVersion())

    result.current.mutate({
      guardrailIdentifier: "gr-1",
      description: "Snapshot before rollout",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
      description: "Snapshot before rollout",
    })
  })

  it("invalidates the versions for that guardrail on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateGuardrailVersion(),
      queryClient,
    )

    result.current.mutate({ guardrailIdentifier: "gr-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock", "guardrails", "gr-1", "versions"],
    })
  })
})
