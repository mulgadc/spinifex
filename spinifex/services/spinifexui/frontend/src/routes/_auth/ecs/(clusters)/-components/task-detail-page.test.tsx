import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { TaskDetailPage } from "./task-detail-page"

const TASK_ID = "abc123def456"
const TASK_ARN = `arn:aws:ecs:ap-southeast-2:123456789012:task/web/${TASK_ID}`

function seedTask(details: { name: string; value: string }[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(
    ["ecs", "clusters", "web", "tasks"],
    [
      {
        taskArn: TASK_ARN,
        lastStatus: "RUNNING",
        desiredStatus: "RUNNING",
        taskDefinitionArn:
          "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3",
        launchType: "EC2",
        group: "service:web-svc",
        attachments: [
          {
            type: "ElasticNetworkInterface",
            details,
          },
        ],
        containers: [
          {
            name: "app",
            image: "nginx:latest",
            lastStatus: "RUNNING",
            healthStatus: "HEALTHY",
          },
        ],
      },
    ],
  )
  return qc
}

describe("TaskDetailPage", () => {
  it("shows private and public IP when both are present", () => {
    renderWithClient(
      <TaskDetailPage clusterName="web" taskId={TASK_ID} />,
      seedTask([
        { name: "networkInterfaceId", value: "eni-0abc" },
        { name: "privateIPv4Address", value: "10.0.1.5" },
        { name: "publicIPv4Address", value: "54.1.2.3" },
        { name: "subnetId", value: "subnet-123" },
        { name: "macAddress", value: "0a:1b:2c:3d:4e:5f" },
      ]),
    )

    expect(screen.getByText("Private IP")).toBeInTheDocument()
    expect(screen.getByText("10.0.1.5")).toBeInTheDocument()
    expect(screen.getByText("Public IP")).toBeInTheDocument()
    expect(screen.getByText("54.1.2.3")).toBeInTheDocument()
    expect(screen.getByText("eni-0abc")).toBeInTheDocument()
  })

  it("renders an em dash for an absent public IP", () => {
    renderWithClient(
      <TaskDetailPage clusterName="web" taskId={TASK_ID} />,
      seedTask([
        { name: "networkInterfaceId", value: "eni-0abc" },
        { name: "privateIPv4Address", value: "10.0.1.5" },
        { name: "subnetId", value: "subnet-123" },
      ]),
    )

    expect(screen.getByText("10.0.1.5")).toBeInTheDocument()
    expect(screen.getByText("Public IP")).toBeInTheDocument()
    expect(screen.queryByText("54.1.2.3")).not.toBeInTheDocument()
    expect(screen.getAllByText("—").length).toBeGreaterThan(0)
  })

  it("lists containers with status and health", () => {
    renderWithClient(
      <TaskDetailPage clusterName="web" taskId={TASK_ID} />,
      seedTask([{ name: "privateIPv4Address", value: "10.0.1.5" }]),
    )

    expect(screen.getByText("app")).toBeInTheDocument()
    expect(screen.getByText("nginx:latest")).toBeInTheDocument()
    expect(screen.getByText("HEALTHY")).toBeInTheDocument()
  })

  it("shows a not-found message when the task is missing", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "clusters", "web", "tasks"], [])
    renderWithClient(<TaskDetailPage clusterName="web" taskId={TASK_ID} />, qc)

    expect(screen.getByText("Task not found.")).toBeInTheDocument()
  })
})
