import { DeleteSecurityGroupCommand } from "@aws-sdk/client-ec2"
import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import {
  ec2InstanceQueryOptions,
  ec2InstancesQueryOptions,
} from "@/queries/ec2"
import type { CreateVpcWizardFormData } from "@/types/ec2"

import {
  buildLaunchTemplateData,
  useAssociateIamInstanceProfile,
  useAttachVolume,
  useAuthorizeSecurityGroupEgress,
  useAuthorizeSecurityGroupIngress,
  useCopySnapshot,
  useCreateImage,
  useCreateInstance,
  useCreateKeyPair,
  useCreateLaunchTemplate,
  useCreateLaunchTemplateVersion,
  useCreatePlacementGroup,
  useCreateSecurityGroup,
  useCreateSnapshot,
  useCreateSubnet,
  useCreateVolume,
  useCreateVpc,
  useCreateVpcWizard,
  useDeleteKeyPair,
  useDeleteLaunchTemplate,
  useDeleteLaunchTemplateVersions,
  useDisassociateIamInstanceProfile,
  useDeletePlacementGroup,
  useDeleteSecurityGroup,
  useDeleteSnapshot,
  useDeleteSubnet,
  useDeleteVolume,
  useDeleteVpc,
  useDetachVolume,
  useGetConsoleOutput,
  useImportKeyPair,
  useModifyInstanceAttribute,
  useModifyLaunchTemplate,
  useModifyVolume,
  useRebootInstance,
  useRevokeSecurityGroupEgress,
  useRevokeSecurityGroupIngress,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "./ec2"

/* oxlint-disable promise/avoid-new -- holding one SDK call open is the only way to order it after a sibling's failure */
interface HeldResponse<T> {
  promise: Promise<T>
  release: (value: T) => void
}

function heldResponse<T>(): HeldResponse<T> {
  let release!: (value: T) => void

  const promise = new Promise<T>((resolve) => {
    release = resolve
  })

  return { promise, release }
}

const settleTimers = async () =>
  await new Promise((resolve) => {
    setTimeout(resolve, 10)
  })
/* oxlint-enable promise/avoid-new */

describe("useStartInstance", () => {
  it("sends StartInstancesCommand with the instance ID", async () => {
    const { result } = renderMutationHook(() => useStartInstance())

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledOnce()

    const command = mockSend.mock.calls[0]?.[0]
    expect(command.input).toStrictEqual({ InstanceIds: ["i-abc123"] })
  })

  it("marks the cached instance list and detail stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(ec2InstancesQueryOptions.queryKey, {
      $metadata: {},
    })
    queryClient.setQueryData(ec2InstanceQueryOptions("i-abc123").queryKey, {
      $metadata: {},
    })
    const { result } = renderMutationHook(() => useStartInstance(), queryClient)

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(ec2InstancesQueryOptions.queryKey)
        ?.isInvalidated,
    ).toBeTruthy()
    expect(
      queryClient.getQueryState(ec2InstanceQueryOptions("i-abc123").queryKey)
        ?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useStopInstance", () => {
  it("sends StopInstancesCommand with the instance ID", async () => {
    const { result } = renderMutationHook(() => useStopInstance())

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useTerminateInstance", () => {
  it("sends TerminateInstancesCommand with the instance ID", async () => {
    const { result } = renderMutationHook(() => useTerminateInstance())

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useCreateInstance", () => {
  it("sends RunInstancesCommand with form data", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 2,
      subnetId: "subnet-1",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toMatchObject({
      ImageId: "ami-123",
      InstanceType: "t2.micro",
      KeyName: "my-key",
      MinCount: 2,
      MaxCount: 2,
      SubnetId: "subnet-1",
    })
  })

  it("omits SubnetId when empty string", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      subnetId: "",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.SubnetId).toBeUndefined()
  })

  it("includes Placement when placementGroupName is provided", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      placementGroupName: "my-group",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.Placement).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("omits Placement when placementGroupName is empty", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      placementGroupName: "",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.Placement).toBeUndefined()
  })

  it("omits BlockDeviceMappings when no storage fields are set", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings,
    ).toBeUndefined()
  })

  it("includes BlockDeviceMappings when root volume size is set", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
      rootVolumeSize: 100,
      rootVolumeType: "gp3",
      rootDeleteOnTermination: true,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings).toStrictEqual(
      [
        {
          DeviceName: "/dev/sda1",
          Ebs: {
            VolumeSize: 100,
            VolumeType: "gp3",
            DeleteOnTermination: true,
          },
        },
      ],
    )
  })

  it("includes only the storage fields the user set", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
      rootDeleteOnTermination: false,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings).toStrictEqual(
      [
        {
          DeviceName: "/dev/sda1",
          Ebs: {
            DeleteOnTermination: false,
          },
        },
      ],
    )
  })

  it("launches from a template with LaunchTemplate and count", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      launchTemplateId: "lt-123",
      launchTemplateVersion: "3",
      count: 2,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.LaunchTemplate).toStrictEqual({
      LaunchTemplateId: "lt-123",
      Version: "3",
    })
    expect(input.MinCount).toBe(2)
    expect(input.MaxCount).toBe(2)
    expect(input.ImageId).toBeUndefined()
    expect(input.InstanceType).toBeUndefined()
  })

  it("defaults the template version to $Default", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({ launchTemplateId: "lt-123", count: 1 })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.LaunchTemplate).toStrictEqual({
      LaunchTemplateId: "lt-123",
      Version: "$Default",
    })
  })

  it("omits LaunchTemplate for a direct launch", async () => {
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.LaunchTemplate).toBeUndefined()
  })
})

describe("useCreateInstance in create-group mode", () => {
  const createModeParams = {
    imageId: "ami-123",
    instanceType: "t2.micro",
    count: 1,
    securityGroupMode: "create" as const,
    newSgName: "launch-wizard-1",
    allowSsh: true,
  }

  it("deletes the new group when RunInstances fails", async () => {
    const launchError = new Error("no capacity")
    mockSend
      .mockResolvedValueOnce({ GroupId: "sg-new" })
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(launchError)
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate(createModeParams)

    await waitFor(() => {
      expect(result.current.error).toBe(launchError)
    })
    expect(mockSend).toHaveBeenCalledTimes(4)
    expect(mockSend.mock.calls[3]?.[0]).toBeInstanceOf(
      DeleteSecurityGroupCommand,
    )
    expect(mockSend.mock.calls[3]?.[0].input).toStrictEqual({
      GroupId: "sg-new",
    })
  })

  it("deletes the new group when authorizing its ingress fails", async () => {
    mockSend
      .mockResolvedValueOnce({ GroupId: "sg-new" })
      .mockRejectedValueOnce(new Error("bad rule"))
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate(createModeParams)

    await waitFor(() => {
      expect(result.current.isError).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(3)
    expect(mockSend.mock.calls[2]?.[0]).toBeInstanceOf(
      DeleteSecurityGroupCommand,
    )
  })

  it("reports the launch error when the cleanup delete also fails", async () => {
    const launchError = new Error("no capacity")
    mockSend
      .mockResolvedValueOnce({ GroupId: "sg-new" })
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(launchError)
      .mockRejectedValueOnce(new Error("delete failed"))
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate(createModeParams)

    await waitFor(() => {
      expect(result.current.error).toBe(launchError)
    })
  })

  it("keeps the new group when the launch succeeds", async () => {
    mockSend.mockResolvedValueOnce({ GroupId: "sg-new" })
    const { result } = renderMutationHook(() => useCreateInstance())

    result.current.mutate(createModeParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(3)
    expect(mockSend.mock.calls[2]?.[0].input.SecurityGroupIds).toStrictEqual([
      "sg-new",
    ])
  })
})

describe("buildLaunchTemplateData", () => {
  it("maps the core launch fields", () => {
    const data = buildLaunchTemplateData({
      imageId: "ami-1",
      instanceType: "t2.micro",
      keyName: "my-key",
    })

    expect(data).toStrictEqual({
      ImageId: "ami-1",
      InstanceType: "t2.micro",
      KeyName: "my-key",
    })
  })

  it("expresses a subnet as the primary network interface", () => {
    const data = buildLaunchTemplateData({
      imageId: "ami-1",
      instanceType: "t2.micro",
      subnetId: "subnet-1",
      securityGroupIds: ["sg-1", "sg-2"],
    })

    expect(data.NetworkInterfaces).toStrictEqual([
      { DeviceIndex: 0, SubnetId: "subnet-1", Groups: ["sg-1", "sg-2"] },
    ])
    expect(data.SecurityGroupIds).toBeUndefined()
  })

  it("uses top-level SecurityGroupIds when there is no subnet", () => {
    const data = buildLaunchTemplateData({
      imageId: "ami-1",
      instanceType: "t2.micro",
      securityGroupIds: ["sg-1"],
    })

    expect(data.SecurityGroupIds).toStrictEqual(["sg-1"])
    expect(data.NetworkInterfaces).toBeUndefined()
  })

  it("maps the root volume to BlockDeviceMappings", () => {
    const data = buildLaunchTemplateData({
      imageId: "ami-1",
      instanceType: "t2.micro",
      rootVolumeSize: 30,
      rootVolumeType: "gp3",
    })

    expect(data.BlockDeviceMappings).toStrictEqual([
      { DeviceName: "/dev/sda1", Ebs: { VolumeSize: 30, VolumeType: "gp3" } },
    ])
  })

  it("base64-encodes user data", () => {
    const data = buildLaunchTemplateData({
      imageId: "ami-1",
      instanceType: "t2.micro",
      userData: "#!/bin/bash\necho hi",
    })

    expect(data.UserData).toBe(btoa("#!/bin/bash\necho hi"))
  })
})

describe("useCreateLaunchTemplate", () => {
  it("sends CreateLaunchTemplateCommand with name and data", async () => {
    const { result } = renderMutationHook(() => useCreateLaunchTemplate())

    result.current.mutate({
      launchTemplateName: "web",
      versionDescription: "initial",
      imageId: "ami-1",
      instanceType: "t2.micro",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.LaunchTemplateName).toBe("web")
    expect(input.VersionDescription).toBe("initial")
    expect(input.LaunchTemplateData).toStrictEqual({
      ImageId: "ami-1",
      InstanceType: "t2.micro",
    })
  })

  it("invalidates the launch templates query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateLaunchTemplate(),
      queryClient,
    )

    result.current.mutate({
      launchTemplateName: "web",
      imageId: "ami-1",
      instanceType: "t2.micro",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "launchTemplates"],
    })
  })
})

describe("useCreateLaunchTemplateVersion", () => {
  it("sends the command with source version and id", async () => {
    const { result } = renderMutationHook(() =>
      useCreateLaunchTemplateVersion(),
    )

    result.current.mutate({
      launchTemplateId: "lt-1",
      sourceVersion: "2",
      versionDescription: "bump",
      imageId: "ami-2",
      instanceType: "t2.small",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.LaunchTemplateId).toBe("lt-1")
    expect(input.SourceVersion).toBe("2")
    expect(input.VersionDescription).toBe("bump")
    expect(input.LaunchTemplateData.ImageId).toBe("ami-2")
  })

  it("invalidates the list and the versions query", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateLaunchTemplateVersion(),
      queryClient,
    )

    result.current.mutate({
      launchTemplateId: "lt-1",
      imageId: "ami-2",
      instanceType: "t2.small",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "launchTemplates"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "launchTemplates", "lt-1", "versions"],
    })
  })
})

describe("useModifyLaunchTemplate", () => {
  it("sends ModifyLaunchTemplateCommand with the default version", async () => {
    const { result } = renderMutationHook(() => useModifyLaunchTemplate())

    result.current.mutate({ launchTemplateId: "lt-1", defaultVersion: "4" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LaunchTemplateId: "lt-1",
      DefaultVersion: "4",
    })
  })
})

describe("useDeleteLaunchTemplateVersions", () => {
  it("sends DeleteLaunchTemplateVersionsCommand with the versions", async () => {
    const { result } = renderMutationHook(() =>
      useDeleteLaunchTemplateVersions(),
    )

    result.current.mutate({ launchTemplateId: "lt-1", versions: ["2", "3"] })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LaunchTemplateId: "lt-1",
      Versions: ["2", "3"],
    })
  })

  it("fails when a version comes back in the unsuccessful list", async () => {
    mockSend.mockResolvedValueOnce({
      SuccessfullyDeletedLaunchTemplateVersions: [],
      UnsuccessfullyDeletedLaunchTemplateVersions: [
        {
          VersionNumber: 2,
          ResponseError: {
            Code: "unexpectedError",
            Message: "storage unavailable",
          },
        },
      ],
    })

    const { result } = renderMutationHook(() =>
      useDeleteLaunchTemplateVersions(),
    )

    result.current.mutate({ launchTemplateId: "lt-1", versions: ["2"] })

    await waitFor(() => {
      expect(result.current.isError).toBeTruthy()
    })
    expect(result.current.error?.message).toBe(
      "Version 2 was not deleted: storage unavailable",
    )
  })
})

describe("useDeleteLaunchTemplate", () => {
  it("sends DeleteLaunchTemplateCommand with the id", async () => {
    const { result } = renderMutationHook(() => useDeleteLaunchTemplate())

    result.current.mutate("lt-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LaunchTemplateId: "lt-1",
    })
  })
})

describe("useImportKeyPair", () => {
  it("strips the comment from an SSH public key", async () => {
    const { result } = renderMutationHook(() => useImportKeyPair())

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "ssh-rsa AAAAB3Nza... user@host",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.KeyName).toBe("my-key")

    const decoded = new TextDecoder().decode(input.PublicKeyMaterial)
    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
    expect(decoded).not.toContain("user@host")
  })

  it("handles keys without comments", async () => {
    const { result } = renderMutationHook(() => useImportKeyPair())

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "ssh-rsa AAAAB3Nza...",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })

    const decoded = new TextDecoder().decode(
      mockSend.mock.calls[0]?.[0].input.PublicKeyMaterial,
    )

    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
  })

  it("handles extra whitespace in key", async () => {
    const { result } = renderMutationHook(() => useImportKeyPair())

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "  ssh-rsa   AAAAB3Nza...   user@host  ",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })

    const decoded = new TextDecoder().decode(
      mockSend.mock.calls[0]?.[0].input.PublicKeyMaterial,
    )

    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
  })
})

describe("useRebootInstance", () => {
  it("sends RebootInstancesCommand with the instance ID", async () => {
    const { result } = renderMutationHook(() => useRebootInstance())

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useCreateKeyPair", () => {
  it("sends CreateKeyPairCommand with the selected key type", async () => {
    mockSend.mockResolvedValueOnce({ KeyMaterial: "-----BEGIN RSA-----" })
    const { result } = renderMutationHook(() => useCreateKeyPair())

    result.current.mutate({ keyName: "my-key", keyType: "rsa" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "rsa",
    })
  })

  it("passes ed25519 through rather than forcing rsa", async () => {
    mockSend.mockResolvedValueOnce({ KeyMaterial: "-----BEGIN OPENSSH-----" })
    const { result } = renderMutationHook(() => useCreateKeyPair())

    result.current.mutate({ keyName: "my-key", keyType: "ed25519" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "ed25519",
    })
  })

  it("fails when the API omits the private key material", async () => {
    mockSend.mockResolvedValueOnce({})
    const { result } = renderMutationHook(() => useCreateKeyPair())

    result.current.mutate({ keyName: "my-key", keyType: "rsa" })

    await waitFor(() => {
      expect(result.current.isError).toBeTruthy()
    })
    expect(result.current.error?.message).toContain("no private key")
  })
})

describe("useDeleteKeyPair", () => {
  it("sends DeleteKeyPairCommand with key pair ID", async () => {
    const { result } = renderMutationHook(() => useDeleteKeyPair())

    result.current.mutate("kp-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyPairId: "kp-abc123",
    })
  })
})

describe("useCreateVolume", () => {
  it("sends CreateVolumeCommand with size and availability zone", async () => {
    const { result } = renderMutationHook(() => useCreateVolume())

    result.current.mutate({ size: 100, availabilityZone: "us-east-1a" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Size: 100,
      AvailabilityZone: "us-east-1a",
      VolumeType: "gp3",
    })
  })
})

describe("useModifyVolume", () => {
  it("sends ModifyVolumeCommand with volume ID and new size", async () => {
    const { result } = renderMutationHook(() => useModifyVolume())

    result.current.mutate({ volumeId: "vol-123", size: 200 })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      Size: 200,
    })
  })
})

describe("useDeleteVolume", () => {
  it("sends DeleteVolumeCommand with volume ID", async () => {
    const { result } = renderMutationHook(() => useDeleteVolume())

    result.current.mutate("vol-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
    })
  })
})

describe("useCreateSnapshot", () => {
  it("sends CreateSnapshotCommand with volume ID and description", async () => {
    const { result } = renderMutationHook(() => useCreateSnapshot())

    result.current.mutate({ volumeId: "vol-123", description: "backup" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      Description: "backup",
    })
  })

  it("omits Description when empty", async () => {
    const { result } = renderMutationHook(() => useCreateSnapshot())

    result.current.mutate({ volumeId: "vol-123", description: "" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.Description).toBeUndefined()
  })
})

describe("useDeleteSnapshot", () => {
  it("sends DeleteSnapshotCommand with snapshot ID", async () => {
    const { result } = renderMutationHook(() => useDeleteSnapshot())

    result.current.mutate("snap-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SnapshotId: "snap-123",
    })
  })
})

describe("useCopySnapshot", () => {
  it("sends CopySnapshotCommand with source details", async () => {
    const { result } = renderMutationHook(() => useCopySnapshot())

    result.current.mutate({
      sourceSnapshotId: "snap-123",
      sourceRegion: "us-east-1",
      description: "copy",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SourceSnapshotId: "snap-123",
      SourceRegion: "us-east-1",
      Description: "copy",
    })
  })
})

describe("useAttachVolume", () => {
  it("sends AttachVolumeCommand with volume, instance, and device", async () => {
    const { result } = renderMutationHook(() => useAttachVolume())

    result.current.mutate({
      volumeId: "vol-123",
      instanceId: "i-abc",
      device: "/dev/sdf",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      InstanceId: "i-abc",
      Device: "/dev/sdf",
    })
  })
})

describe("useDetachVolume", () => {
  it("sends DetachVolumeCommand with volume, instance, and force", async () => {
    const { result } = renderMutationHook(() => useDetachVolume())

    result.current.mutate({
      volumeId: "vol-123",
      instanceId: "i-abc",
      force: true,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      InstanceId: "i-abc",
      Force: true,
    })
  })
})

describe("useModifyInstanceAttribute", () => {
  it("sends ModifyInstanceAttributeCommand with instance type", async () => {
    const { result } = renderMutationHook(() => useModifyInstanceAttribute())

    result.current.mutate({
      instanceId: "i-abc",
      instanceType: "t3.large",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc",
      InstanceType: { Value: "t3.large" },
    })
  })
})

describe("useGetConsoleOutput", () => {
  it("sends GetConsoleOutputCommand with instance ID", async () => {
    const { result } = renderMutationHook(() => useGetConsoleOutput())

    result.current.mutate("i-abc123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc123",
    })
  })
})

describe("useCreateImage", () => {
  it("sends CreateImageCommand with instance ID and name", async () => {
    const { result } = renderMutationHook(() => useCreateImage())

    result.current.mutate({
      instanceId: "i-abc",
      name: "my-image",
      description: "test image",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc",
      Name: "my-image",
      Description: "test image",
    })
  })
})

describe("useCreateVpc", () => {
  it("includes TagSpecifications when name is provided", async () => {
    const { result } = renderMutationHook(() => useCreateVpc())

    result.current.mutate({ cidrBlock: "10.0.0.0/16", name: "my-vpc" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      CidrBlock: "10.0.0.0/16",
      TagSpecifications: [
        {
          ResourceType: "vpc",
          Tags: [{ Key: "Name", Value: "my-vpc" }],
        },
      ],
    })
  })

  it("sends user tags after the Name tag", async () => {
    const { result } = renderMutationHook(() => useCreateVpc())

    result.current.mutate({
      cidrBlock: "10.0.0.0/16",
      name: "my-vpc",
      tags: [{ key: "Env", value: "prod" }],
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.TagSpecifications).toStrictEqual([
      {
        ResourceType: "vpc",
        Tags: [
          { Key: "Name", Value: "my-vpc" },
          { Key: "Env", Value: "prod" },
        ],
      },
    ])
  })

  it("omits TagSpecifications when name is empty", async () => {
    const { result } = renderMutationHook(() => useCreateVpc())

    result.current.mutate({ cidrBlock: "10.0.0.0/16", name: "" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.TagSpecifications).toBeUndefined()
  })
})

describe("useDeleteVpc", () => {
  it("sends DeleteVpcCommand with VPC ID", async () => {
    const { result } = renderMutationHook(() => useDeleteVpc())

    result.current.mutate("vpc-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VpcId: "vpc-123",
    })
  })
})

describe("useCreateSubnet", () => {
  it("sends CreateSubnetCommand with VPC ID and CIDR block", async () => {
    const { result } = renderMutationHook(() => useCreateSubnet())

    result.current.mutate({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
      availabilityZone: "us-east-1a",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VpcId: "vpc-123",
      CidrBlock: "10.0.1.0/24",
      AvailabilityZone: "us-east-1a",
    })
  })

  it("calls ModifySubnetAttribute when mapPublicIpOnLaunch is set", async () => {
    mockSend.mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-abc" } })
    const { result } = renderMutationHook(() => useCreateSubnet())

    result.current.mutate({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
      mapPublicIpOnLaunch: true,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(2)
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({
      SubnetId: "subnet-abc",
      MapPublicIpOnLaunch: { Value: true },
    })
  })

  it("skips ModifySubnetAttribute when mapPublicIpOnLaunch is false", async () => {
    mockSend.mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-abc" } })
    const { result } = renderMutationHook(() => useCreateSubnet())

    result.current.mutate({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
      mapPublicIpOnLaunch: false,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledOnce()
  })
})

describe("useDeleteSubnet", () => {
  it("sends DeleteSubnetCommand with subnet ID", async () => {
    const { result } = renderMutationHook(() => useDeleteSubnet())

    result.current.mutate("subnet-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SubnetId: "subnet-123",
    })
  })
})

describe("useCreatePlacementGroup", () => {
  it("sends CreatePlacementGroupCommand with group name and strategy", async () => {
    const { result } = renderMutationHook(() => useCreatePlacementGroup())

    result.current.mutate({ groupName: "my-group", strategy: "spread" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Strategy: "spread",
    })
  })

  it("invalidates placementGroups query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreatePlacementGroup(),
      queryClient,
    )

    result.current.mutate({ groupName: "my-group", strategy: "cluster" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "placementGroups"],
    })
  })
})

describe("useDeletePlacementGroup", () => {
  it("sends DeletePlacementGroupCommand with group name", async () => {
    const { result } = renderMutationHook(() => useDeletePlacementGroup())

    result.current.mutate("my-group")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("invalidates placementGroups query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeletePlacementGroup(),
      queryClient,
    )

    result.current.mutate("my-group")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "placementGroups"],
    })
  })
})

describe("useCreateSecurityGroup", () => {
  it("sends CreateSecurityGroupCommand with form data", async () => {
    const { result } = renderMutationHook(() => useCreateSecurityGroup())

    result.current.mutate({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "web-sg",
      Description: "Allow web traffic",
      VpcId: "vpc-123",
    })
  })

  it("invalidates securityGroups query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateSecurityGroup(),
      queryClient,
    )

    result.current.mutate({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
  })
})

describe("useDeleteSecurityGroup", () => {
  it("sends DeleteSecurityGroupCommand with group ID", async () => {
    const { result } = renderMutationHook(() => useDeleteSecurityGroup())

    result.current.mutate("sg-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
    })
  })

  it("invalidates securityGroups query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeleteSecurityGroup(),
      queryClient,
    )

    result.current.mutate("sg-123")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
  })
})

describe("useAuthorizeSecurityGroupIngress", () => {
  const ruleParams = {
    groupId: "sg-123",
    ipProtocol: "tcp",
    fromPort: 443,
    toPort: 443,
    cidrIp: "0.0.0.0/0",
  }

  it("sends AuthorizeSecurityGroupIngressCommand with IpPermissions", async () => {
    const { result } = renderMutationHook(() =>
      useAuthorizeSecurityGroupIngress(),
    )

    result.current.mutate(ruleParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
      IpPermissions: [
        {
          IpProtocol: "tcp",
          FromPort: 443,
          ToPort: 443,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })

  it("invalidates securityGroups queries on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAuthorizeSecurityGroupIngress(),
      queryClient,
    )

    result.current.mutate(ruleParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups", "sg-123"],
    })
  })
})

describe("useAuthorizeSecurityGroupEgress", () => {
  it("sends AuthorizeSecurityGroupEgressCommand with IpPermissions", async () => {
    const { result } = renderMutationHook(() =>
      useAuthorizeSecurityGroupEgress(),
    )

    result.current.mutate({
      groupId: "sg-456",
      ipProtocol: "-1",
      fromPort: -1,
      toPort: -1,
      cidrIp: "0.0.0.0/0",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-456",
      IpPermissions: [
        {
          IpProtocol: "-1",
          FromPort: -1,
          ToPort: -1,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })
})

describe("useRevokeSecurityGroupIngress", () => {
  it("sends RevokeSecurityGroupIngressCommand with IpPermissions", async () => {
    const { result } = renderMutationHook(() => useRevokeSecurityGroupIngress())

    result.current.mutate({
      groupId: "sg-123",
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "10.0.0.0/16",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
      IpPermissions: [
        {
          IpProtocol: "tcp",
          FromPort: 22,
          ToPort: 22,
          IpRanges: [{ CidrIp: "10.0.0.0/16" }],
        },
      ],
    })
  })

  it("uses UserIdGroupPairs when sourceGroupId is given", async () => {
    const { result } = renderMutationHook(() => useRevokeSecurityGroupIngress())

    result.current.mutate({
      groupId: "sg-123",
      ipProtocol: "-1",
      fromPort: 0,
      toPort: 0,
      sourceGroupId: "sg-123",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
      IpPermissions: [
        {
          IpProtocol: "-1",
          FromPort: 0,
          ToPort: 0,
          UserIdGroupPairs: [{ GroupId: "sg-123" }],
        },
      ],
    })
  })

  it("invalidates securityGroups queries on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useRevokeSecurityGroupIngress(),
      queryClient,
    )

    result.current.mutate({
      groupId: "sg-123",
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "10.0.0.0/16",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups", "sg-123"],
    })
  })
})

describe("useRevokeSecurityGroupEgress", () => {
  it("sends RevokeSecurityGroupEgressCommand with IpPermissions", async () => {
    const { result } = renderMutationHook(() => useRevokeSecurityGroupEgress())

    result.current.mutate({
      groupId: "sg-456",
      ipProtocol: "udp",
      fromPort: 53,
      toPort: 53,
      cidrIp: "0.0.0.0/0",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-456",
      IpPermissions: [
        {
          IpProtocol: "udp",
          FromPort: 53,
          ToPort: 53,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })
})

describe("useCreateVpcWizard", () => {
  const baseParams: CreateVpcWizardFormData = {
    mode: "vpc-only",
    namePrefix: "test",
    autoGenerateNames: true,
    cidrBlock: "10.0.0.0/16",
    tenancy: "default",
    publicSubnetCount: 0,
    privateSubnetCount: 0,
    publicSubnetCidrs: [],
    privateSubnetCidrs: [],
    tags: [],
  }

  it("sends only CreateVpcCommand in vpc-only mode", async () => {
    mockSend.mockResolvedValueOnce({
      Vpc: { VpcId: "vpc-111" },
    })
    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate(baseParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input.CidrBlock).toBe("10.0.0.0/16")
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(1)
  })

  it("includes TagSpecifications with auto-generated name", async () => {
    mockSend.mockResolvedValueOnce({
      Vpc: { VpcId: "vpc-111" },
    })
    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      namePrefix: "proj",
      autoGenerateNames: true,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const tags = mockSend.mock.calls[0]?.[0].input.TagSpecifications
    expect(tags?.[0]?.Tags).toContainEqual({
      Key: "Name",
      Value: "proj-vpc",
    })
  })

  it("sends correct command sequence for vpc-and-more with 1 public + 1 private", async () => {
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-pub-1" } })
      // ModifySubnetAttribute (MapPublicIpOnLaunch) for the public subnet
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({
        InternetGateway: { InternetGatewayId: "igw-111" },
      })
      // AttachInternetGateway
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-pub-1" },
      })
      // CreateRoute
      .mockResolvedValueOnce({})
      // AssociateRouteTable (public)
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-priv-1" },
      })
      // AssociateRouteTable (private)
      .mockResolvedValueOnce({})

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(11)
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(6)
    expect(result.current.data?.error).toBeUndefined()
  })

  it("provisions a NAT gateway when natGateway is single", async () => {
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-pub-1" } })
      // ModifySubnetAttribute (public)
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({
        InternetGateway: { InternetGatewayId: "igw-111" },
      })
      // AttachInternetGateway
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ RouteTable: { RouteTableId: "rtb-pub-1" } })
      // CreateRoute (igw) + AssociateRouteTable (public)
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ RouteTable: { RouteTableId: "rtb-priv-1" } })
      // AssociateRouteTable (private)
      .mockResolvedValueOnce({})
      // AllocateAddress -> CreateNatGateway -> CreateRoute (nat)
      .mockResolvedValueOnce({ AllocationId: "eipalloc-1" })
      .mockResolvedValueOnce({ NatGateway: { NatGatewayId: "nat-1" } })
      .mockResolvedValueOnce({})

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
      natGateway: "single",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(14)
    const types = result.current.data?.created.map((r) => r.type)
    expect(types).toContain("Elastic IP")
    expect(types).toContain("NAT Gateway")
    expect(result.current.data?.created).toHaveLength(8)
    expect(result.current.data?.error).toBeUndefined()
  })

  it("skips IGW and public route table when publicSubnetCount is 0", async () => {
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-priv-1" },
      })
      // AssociateRouteTable
      .mockResolvedValueOnce({})

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 0,
      privateSubnetCount: 1,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend).toHaveBeenCalledTimes(4)
    const types = result.current.data?.created.map((r) => r.type)
    expect(types).not.toContain("Internet Gateway")
    expect(types).not.toContain("Public Route Table")
  })

  it("returns partial result with failedStep on error mid-orchestration", async () => {
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-pub-1" } })
      // ModifySubnetAttribute (MapPublicIpOnLaunch) for the public subnet
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(new Error("CIDR conflict"))

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(result.current.data?.error?.message).toBe("CIDR conflict")
    expect(result.current.data?.failedStep).toBe(
      "Failed while creating private subnets",
    )
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(2)
  })

  it("reports every created subnet when a parallel create fails", async () => {
    const secondPrivate = heldResponse<{ Subnet: { SubnetId: string } }>()
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockRejectedValueOnce(new Error("CIDR conflict"))
      .mockImplementationOnce(async () => await secondPrivate.promise)

    const { result } = renderMutationHook(() => useCreateVpcWizard())
    let settled = false
    void result.current
      .mutateAsync({
        ...baseParams,
        mode: "vpc-and-more",
        publicSubnetCount: 0,
        privateSubnetCount: 2,
      })
      .then(() => {
        settled = true
      })

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalledTimes(3)
    })
    await settleTimers()
    expect(settled).toBeFalsy()

    secondPrivate.release({ Subnet: { SubnetId: "subnet-priv-2" } })
    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(result.current.data?.error?.message).toBe("CIDR conflict")
    expect(result.current.data?.created).toContainEqual({
      type: "Private Subnet",
      id: "subnet-priv-2",
    })
  })

  it("fills subnets beyond the custom CIDR list with defaults", async () => {
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-2" } })

    const { result } = renderMutationHook(() => useCreateVpcWizard())
    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 0,
      privateSubnetCount: 2,
      privateSubnetCidrs: ["10.0.200.0/24"],
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[1]?.[0].input.CidrBlock).toBe("10.0.200.0/24")
    expect(mockSend.mock.calls[2]?.[0].input.CidrBlock).toBe("10.0.144.0/20")
  })

  it("returns error when VPC creation fails", async () => {
    mockSend.mockRejectedValueOnce(new Error("Access denied"))

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate(baseParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(result.current.data?.error?.message).toBe("Access denied")
    expect(result.current.data?.failedStep).toBe("Failed while creating VPC")
    expect(result.current.data?.vpcId).toBeUndefined()
    expect(result.current.data?.created).toHaveLength(0)
  })

  it("returns error when VPC is created but no ID returned", async () => {
    mockSend.mockResolvedValueOnce({ Vpc: {} })

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate(baseParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(result.current.data?.error?.message).toContain(
      "no VPC ID was returned",
    )
    expect(result.current.data?.failedStep).toBe("Failed while creating VPC")
  })

  it("invalidates related queries on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    mockSend.mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })

    const { result } = renderMutationHook(
      () => useCreateVpcWizard(),
      queryClient,
    )

    result.current.mutate(baseParams)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "vpcs"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "subnets"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "internetGateways"],
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "routeTables"] })
  })

  it("propagates extra tags to all resources", async () => {
    mockSend.mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })

    const { result } = renderMutationHook(() => useCreateVpcWizard())

    result.current.mutate({
      ...baseParams,
      tags: [{ key: "Env", value: "prod" }],
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const tags = mockSend.mock.calls[0]?.[0].input.TagSpecifications?.[0]?.Tags
    expect(tags).toContainEqual({ Key: "Env", Value: "prod" })
  })
})

describe("useAssociateIamInstanceProfile", () => {
  it("sends AssociateIamInstanceProfileCommand with instance and profile", async () => {
    const { result } = renderMutationHook(() =>
      useAssociateIamInstanceProfile(),
    )

    result.current.mutate({
      instanceId: "i-abc123",
      instanceProfileName: "my-profile",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc123",
      IamInstanceProfile: { Name: "my-profile" },
    })
  })

  it("invalidates associations and instances on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAssociateIamInstanceProfile(),
      queryClient,
    )

    result.current.mutate({
      instanceId: "i-abc123",
      instanceProfileName: "my-profile",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "iam-instance-profile-associations", "i-abc123"],
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "instances"] })
  })
})

describe("useDisassociateIamInstanceProfile", () => {
  it("sends DisassociateIamInstanceProfileCommand with associationId", async () => {
    const { result } = renderMutationHook(() =>
      useDisassociateIamInstanceProfile(),
    )

    result.current.mutate({
      associationId: "iip-assoc-1",
      instanceId: "i-abc123",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      AssociationId: "iip-assoc-1",
    })
  })
})
