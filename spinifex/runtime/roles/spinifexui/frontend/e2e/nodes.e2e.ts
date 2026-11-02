import { expect, json, type JsonValue, NODES, test } from "./console"

const HOSTS = ["bottlebrush", "ironbark", "casuarina"]

// Three bare-metal nodes shaped like dev-prod: 1 GbE onto the office network
// with no LLDP, and 25 GbE onto a switch that names its ports.
const CLUSTER = {
  cluster_mode: "multi-node",
  nodes: HOSTS.map((node, i) => ({
    ...NODES.nodes[0],
    node,
    host: `192.168.1.${14 + i}`,
    az: "ap-southeast-2-az1",
    status: i === 2 ? "NotReady" : "Ready",
    vm_count: i,
    alloc_vcpu: 4 * i,
    alloc_mem_gb: 8 * i,
    nats_role: i === 0 ? "leader" : "follower",
    instance_types: [
      { name: "t3.micro", vcpu: 2, memory_gb: 1, available: 10 + i },
      { name: "m5.4xlarge", vcpu: 16, memory_gb: 64, available: 1 },
      { name: "c5.large", vcpu: 2, memory_gb: 4, available: 30 },
    ],
  })),
}

const FLAGS =
  "fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 ht syscall nx mmxext fxsr_opt pdpe1gb rdtscp lm constant_tsc rep_good nopl nonstop_tsc cpuid extd_apicid aperfmperf pni pclmulqdq monitor ssse3 fma cx16 pcid sse4_1 sse4_2 x2apic movbe popcnt aes xsave avx f16c rdrand lahf_lm cmp_legacy svm extapic cr8_legacy abm sse4a misalignsse 3dnowprefetch osvw ibs skinit wdt tce topoext perfctr_core perfctr_nb bpext perfctr_llc mwaitx cpb cat_l3 cdp_l3 hw_pstate ssbd mba ibrs ibpb stibp vmmcall fsgsbase bmi1 avx2 smep bmi2 erms invpcid cqm rdt_a rdseed adx smap clflushopt clwb sha_ni xsaveopt xsavec xgetbv1 xsaves cqm_llc cqm_occup_llc cqm_mbm_total cqm_mbm_local clzero irperf xsaveerptr rdpru wbnoinvd amd_ppin arat npt lbrv svm_lock nrip_save tsc_scale vmcb_clean flushbyasid decodeassists pausefilter pfthreshold v_vmsave_vmload vgif v_spec_ctrl umip pku ospke vaes vpclmulqdq rdpid overflow_recov succor smca sme sev sev_es".split(
    " ",
  )

const FACTS = {
  nodes: HOSTS.map((node, i): JsonValue => ({
    node,
    form_factor: "rack-server",
    system: {
      vendor: "GIGABYTE",
      product: "R182-Z92-00",
      chassis_type: "Rack Mount Chassis",
    },
    os: {
      name: "Debian GNU/Linux 13 (trixie)",
      kernel: "6.12.113",
      arch: "x86_64",
    },
    cpu: {
      model: "AMD EPYC 7713 64-Core Processor",
      sockets: 2,
      cores: 128,
      threads: 256,
      virtualization: "AMD-V",
      features: ["avx2", "aes", "sev", "sme"],
      flags: FLAGS,
    },
    memory: {
      total_bytes: 549_755_813_888,
      slots_used: 16,
      slots_total: 32,
      groups: [],
    },
    nics: [
      { name: "eno1", state: "up", speed_mbps: 1000, planes: ["wan"] },
      {
        name: "enp193s0f1np1",
        state: "up",
        speed_mbps: 25_000,
        planes: ["lan", "vpc"],
        neighbor: {
          chassis_id: "d0:ea:11:a8:91:4c",
          system_name: "CRS518",
          system_description: "RouterOS CRS518-16XS-2XQ",
          port_id: `sfp28-${i + 1}`,
        },
      },
      { name: "enp193s0f0np0", state: "down", speed_mbps: 25_000 },
    ],
    planes: [],
    sources: { dmi: "ok", lldp: "ok" },
  })),
}

test.beforeEach(({ api }) => {
  api.handle("GetNodes", () => json(CLUSTER))
  api.handle("GetNodeFacts", () => json(FACTS))
  api.handle("DescribeNetworkTests", () => json({ tests: [] }))
})

test.describe("Nodes", () => {
  test("the dashboard lists the nodes and a row opens one", async ({
    page,
  }) => {
    await page.goto("/")

    const nodes = page.getByRole("table", { name: "Cluster nodes" })
    await expect(nodes.getByRole("link", { name: "ironbark" })).toBeVisible()
    await expect(nodes.getByText("notready")).toHaveAttribute(
      "data-tone",
      "danger",
    )

    await nodes.getByText("192.168.1.15").click()
    await expect(page).toHaveURL(/\/nodes\?selected=ironbark/u)
  })

  test("sorts instance type capacity by most available first", async ({
    page,
  }) => {
    await page.goto("/nodes")

    const rows = page
      .getByRole("table", { name: "Instance type capacity" })
      .getByRole("row")

    await expect(rows.nth(1)).toContainText("c5.large")
    await expect(rows.nth(3)).toContainText("m5.4xlarge")
  })

  test("lists every CPU flag behind the features tip", async ({ page }) => {
    await page.goto("/nodes?selected=bottlebrush")

    await page.getByRole("button", { name: "About Features" }).hover()
    await expect(page.getByText(/sev_es/u)).toBeVisible()
  })

  test("brings a link's ports forward on hover", async ({ page }) => {
    await page.goto("/nodes/topology")

    const label = page.getByTestId("port-ironbark:enp193s0f1np1")
    await expect(label).toContainText("enp193s0f1np1 ↔ sfp28-2")
    await expect(page.locator(".react-flow__attribution")).toHaveCount(0)

    await page.getByText("ironbark", { exact: true }).hover()
    await expect(label).toHaveAttribute("data-focus", "on")
    await expect(
      page.getByTestId("port-bottlebrush:enp193s0f1np1"),
    ).toHaveAttribute("data-focus", "off")
  })
})
