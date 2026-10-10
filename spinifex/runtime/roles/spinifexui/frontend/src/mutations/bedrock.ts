import {
  CreateGuardrailCommand,
  CreateGuardrailVersionCommand,
  DeleteGuardrailCommand,
  UpdateGuardrailCommand,
} from "@aws-sdk/client-bedrock"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getBedrockClient } from "@/lib/awsClient"
import {
  formToCreateInput,
  formToUpdateInput,
  type GuardrailFormData,
} from "@/types/bedrock"

const GUARDRAILS_KEY = ["bedrock", "guardrails"]

function guardrailKey(guardrailIdentifier: string) {
  return [...GUARDRAILS_KEY, guardrailIdentifier]
}

function guardrailVersionsKey(guardrailIdentifier: string) {
  return [...guardrailKey(guardrailIdentifier), "versions"]
}

export function useCreateGuardrail() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (data: GuardrailFormData) => {
      const command = new CreateGuardrailCommand(formToCreateInput(data))
      return await getBedrockClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: GUARDRAILS_KEY })
    },
  })
}

export interface UpdateGuardrailParams {
  guardrailIdentifier: string
  data: GuardrailFormData
}

export function useUpdateGuardrail() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      guardrailIdentifier,
      data,
    }: UpdateGuardrailParams) => {
      const command = new UpdateGuardrailCommand(
        formToUpdateInput(guardrailIdentifier, data),
      )
      return await getBedrockClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({ queryKey: GUARDRAILS_KEY })
      void queryClient.invalidateQueries({
        queryKey: guardrailKey(params.guardrailIdentifier),
      })
    },
  })
}

export function useDeleteGuardrail() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (guardrailIdentifier: string) => {
      const command = new DeleteGuardrailCommand({ guardrailIdentifier })
      return await getBedrockClient().send(command)
    },
    onSuccess: (_data, guardrailIdentifier) => {
      void queryClient.invalidateQueries({ queryKey: GUARDRAILS_KEY })
      void queryClient.invalidateQueries({
        queryKey: guardrailKey(guardrailIdentifier),
      })
    },
  })
}

export interface CreateGuardrailVersionParams {
  guardrailIdentifier: string
  description?: string
}

export function useCreateGuardrailVersion() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      guardrailIdentifier,
      description,
    }: CreateGuardrailVersionParams) => {
      const command = new CreateGuardrailVersionCommand({
        guardrailIdentifier,
        description,
      })
      return await getBedrockClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: guardrailVersionsKey(params.guardrailIdentifier),
      })
    },
  })
}
