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
    Link: ({
      children,
      to,
      params,
    }: {
      children: React.ReactNode
      to?: string
      params?: { taskId?: string }
    }) => <a href={`${to}-${params?.taskId ?? ""}`}>{children}</a>,
  }
})

import { ServiceDetailPage } from "./service-detail-page"

const TASK_ID = "task0001"
const TASK_ARN = `arn:aws:ecs:ap-southeast-2:123456789012:task/web/${TASK_ID}`

function seedService() {
  const qc = createTestQueryClient()
  qc.setQueryData(
    ["ecs", "clusters", "web", "services"],
    [
      {
        serviceName: "web-svc",
        serviceArn:
          "arn:aws:ecs:ap-southeast-2:123456789012:service/web/web-svc",
        status: "ACTIVE",
        desiredCount: 1,
        runningCount: 1,
        pendingCount: 0,
        taskDefinition:
          "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3",
        launchType: "EC2",
        loadBalancers: [
          {
            targetGroupArn:
              "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/tg/abc",
            containerName: "app",
            containerPort: 80,
          },
        ],
        events: [{ id: "e1", message: "service reached steady state" }],
      },
    ],
  )
  qc.setQueryData(
    ["ecs", "clusters", "web", "tasks"],
    [
      {
        taskArn: TASK_ARN,
        lastStatus: "RUNNING",
        group: "service:web-svc",
        attachments: [
          {
            type: "ElasticNetworkInterface",
            details: [
              { name: "privateIPv4Address", value: "10.0.1.5" },
              { name: "publicIPv4Address", value: "54.1.2.3" },
            ],
          },
        ],
      },
    ],
  )
  return qc
}

describe("ServiceDetailPage", () => {
  it("lists member tasks linking to the task detail page", () => {
    renderWithClient(
      <ServiceDetailPage clusterName="web" serviceName="web-svc" />,
      seedService(),
    )

    const link = screen.getByRole("link", { name: TASK_ID })
    expect(link).toBeInTheDocument()
    expect(link.getAttribute("href")).toContain(
      "/ecs/list-clusters/$clusterName/tasks/$taskId",
    )
    expect(link.getAttribute("href")).toContain(TASK_ID)
    expect(screen.getByText("10.0.1.5")).toBeInTheDocument()
    expect(screen.getByText("54.1.2.3")).toBeInTheDocument()
  })

  it("renders load balancers and config", () => {
    renderWithClient(
      <ServiceDetailPage clusterName="web" serviceName="web-svc" />,
      seedService(),
    )

    expect(screen.getByText("app")).toBeInTheDocument()
    expect(screen.getByText("80")).toBeInTheDocument()
    expect(screen.getByText("service reached steady state")).toBeInTheDocument()
  })

  it("shows a not-found message when the service is missing", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "clusters", "web", "services"], [])
    qc.setQueryData(["ecs", "clusters", "web", "tasks"], [])
    renderWithClient(
      <ServiceDetailPage clusterName="web" serviceName="web-svc" />,
      qc,
    )

    expect(screen.getByText("Service not found.")).toBeInTheDocument()
  })
})
