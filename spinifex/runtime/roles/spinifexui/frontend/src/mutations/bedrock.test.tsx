import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getBedrockClient: () => ({ send: mockSend }),
}))

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

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

const FORM: GuardrailFormData = {
  ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
  name: "content-safety",
  blockedInputMessaging: "Blocked input.",
  blockedOutputsMessaging: "Blocked output.",
}

describe("useCreateGuardrail", () => {
  it("sends CreateGuardrailCommand with the form values", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateGuardrail(), { wrapper })

    result.current.mutate(FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("content-safety")
    expect(input.blockedInputMessaging).toBe("Blocked input.")
    expect(input.blockedOutputsMessaging).toBe("Blocked output.")
  })

  it("invalidates the guardrail list on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateGuardrail(), { wrapper })

    result.current.mutate(FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["bedrock", "guardrails"] })
  })
})

describe("useUpdateGuardrail", () => {
  it("sends UpdateGuardrailCommand with the identifier and form values", async () => {
    createQueryClient()
    const { result } = renderHook(() => useUpdateGuardrail(), { wrapper })

    result.current.mutate({ guardrailIdentifier: "gr-1", data: FORM })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.guardrailIdentifier).toBe("gr-1")
    expect(input.name).toBe("content-safety")
  })

  it("invalidates the list and the single guardrail on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useUpdateGuardrail(), { wrapper })

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
    createQueryClient()
    const { result } = renderHook(() => useDeleteGuardrail(), { wrapper })

    result.current.mutate("gr-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
    })
  })

  it("invalidates the list and the single guardrail on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteGuardrail(), { wrapper })

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
    createQueryClient()
    const { result } = renderHook(() => useCreateGuardrailVersion(), {
      wrapper,
    })

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
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateGuardrailVersion(), {
      wrapper,
    })

    result.current.mutate({ guardrailIdentifier: "gr-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock", "guardrails", "gr-1", "versions"],
    })
  })
})
