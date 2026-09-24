# Running Spinifex on Oracle Cloud Infrastructure

Spinifex runs on an OCI instance, and guests launched on it get real, publicly reachable addresses through OCI's own API. This guide is the operator path: what to provision, what to install, what to grant, and what to configure.

**Read the datapath section before you provision anything.** OCI enforces addressing in the virtual network in a way that makes one of Spinifex's three external modes unusable, and the choice is baked in at `spx admin init`. Getting it wrong produces a cluster that looks entirely healthy and drops every guest packet.

Design rationale and the measurements behind each claim: `docs/development/feature/oci-provider-integration.md` in the mulga monorepo.

---

## 1. What OCI does differently, in one page

Three properties of an OCI virtual network shape everything below.

**OCI enforces the source IP per VNIC.** A packet leaving the instance with a source address that is not a registered private-IP object on that VNIC is dropped by the hypervisor. Measured, not inferred: from the same NIC with the same MAC, `ping -I <registered> 8.8.8.8` gets 0% loss and `ping -I <unregistered> 8.8.8.8` gets 100%. This is why an external address pool cannot simply be a range you write in a config file — **every address has to be registered with OCI before it works**, and registering it is exactly what this integration does.

> Pinging the VCN router (`10.200.0.1`) proves nothing. It answers from any source address at ~0.08 ms because the hypervisor replies locally. Always test egress past the subnet.

**A VNIC accepts one MAC; it does not learn MACs.** Every inbound frame arrives addressed to the VNIC's own MAC, whatever address the packet is for. Spinifex's default external datapath advertises each EIP by ARPing from a per-VPC router MAC onto a shared L2 segment, which needs a segment that learns MACs. OCI has none. Enforcement sits in the hypervisor on VM shapes and in the SmartNIC on bare metal, so there is no guest-side workaround.

**A public IP is a second object, and it never appears on the wire.** An OCI public address is a RESERVED public IP that OCI 1:1-NATs upstream onto a **secondary private IP** on the VNIC. The instance only ever sees the private half. Spinifex handles this internally — AWS-facing APIs report the public address, the datapath uses the private one — but it explains why `ip addr` on the host never shows a customer's public IP, and why that is correct rather than a fault.

### The consequence: routed mode is mandatory

| External mode | What goes on the uplink | On OCI |
| --- | --- | --- |
| `pool` (`UplinkModePhysical`, `UplinkModeVeth`) | Per-VPC gateway MAC plus a per-rule external MAC, ARPed onto the segment | **Does not work.** Egress is dropped (wrong source MAC); ingress never reaches OVN (the frame is addressed to the VNIC MAC, which no router port owns). |
| `nat` (`UplinkModeRouted`) | Nothing. The host forwards over an RFC 6598 transit veth and sets no external MAC | **Works.** One MAC identity, and registered source addresses. |

Spinifex **refuses** `source = "oci"` on a pool unless the node is in `nat` mode, and the refusal names this reason.

**Routed mode is currently capped at a single node** (`--nodes=1`). A multi-node Spinifex cluster on OCI is not yet supported.

---

## 2. Prerequisites

### 2.1 Host shape

**Bare metal is the recommendation.** `BM.Standard.E5.192` or similar gives you the hardware directly, no nested virtualisation, full NIC performance, and no hypervisor between your guests and the wire.

**If a VM, size it generously.** Spinifex runs nine services plus every guest on one host, so the host's own footprint is not small.

- **Verify nested virtualisation before anything else.** Oracle's nested-KVM guidance names `VM.Standard3.Flex` (Intel) and `VM.Standard.E5.Flex` (AMD), and states Ampere shapes do not support it. `VM.Standard.E6.Flex` is **not** in that list but was measured working on 2026-09-24 (EPYC 9J45, `/dev/kvm` present, `svm` on all threads, `kvm_amd nested=1`). Treat an unlisted shape as unproven until you have checked.
- **Minimum practical VM:** 8 OCPU / 32 GB. Below that you are sizing for the control plane alone with nothing left for guests.

```bash
# Run this first. If /dev/kvm is absent, stop — change shape, do not continue.
ls -l /dev/kvm
grep -oE 'vmx|svm' /proc/cpuinfo | sort -u
cat /sys/module/kvm_amd/parameters/nested 2>/dev/null \
  || cat /sys/module/kvm_intel/parameters/nested
```

### 2.2 Host image

**OCI has no Debian image.** Ubuntu LTS is the tested path — Ubuntu 26.04 LTS on kernel `7.0.0-1009-oracle` is what this integration was built and proven against. Anything in the tooling that assumes Debian is a known gap, not a supported configuration.

### 2.3 Storage

The boot volume is too small for guest images and block storage. Attach a **block volume** (1 TB is a reasonable start) and mount it at `/var/lib/spinifex`, which puts viperblock, predastore and JetStream on it with no config pointing anywhere unusual.

```bash
sudo mkfs.ext4 /dev/sdb1
UUID=$(sudo blkid -s UUID -o value /dev/sdb1)
echo "UUID=$UUID /var/lib/spinifex ext4 defaults,_netdev,nofail 0 2" | sudo tee -a /etc/fstab
sudo mkdir -p /var/lib/spinifex && sudo mount -a
findmnt /var/lib/spinifex          # verify with findmnt, never ls
```

**`_netdev` is not optional.** An OCI block volume is iSCSI. Without `_netdev` the mount is attempted before `iscsid` has a session, and the boot either hangs or silently lands the whole stack on the small boot disk — which looks identical to a working install until the disk fills.

### 2.4 Networking layout

Two VNICs is the tested shape:

- **Primary VNIC** carries the management plane — the node's advertised address and the OVN encapsulation endpoint.
- **Second VNIC** carries the external datapath. Put it in a **Linux bridge** (`br-wan`) owned by netplan, with the bridge MAC cloned to the VNIC's own MAC.

```yaml
# /etc/netplan/50-cloud-init.yaml (fragment)
bridges:
  br-wan:
    interfaces: [enp1s0]
    macaddress: "02:00:17:01:7e:71"     # the VNIC's own MAC — not optional
    addresses: [10.200.0.29/24]
```

`setup-ovn.sh --wan-bridge=br-wan` detects that the name belongs to a *Linux* bridge and links it to the OVS bridge with a veth pair rather than enslaving the NIC. **That is the correct shape on OCI, not a workaround** — the physical interface never becomes an OVS port, so the VNIC keeps its MAC identity and netplan keeps owning the addresses.

> If you suspect an OVS problem on this host, read `ovs-vsctl --columns=name,error list Interface` and the vswitchd log. **Never trust `ovs-vsctl`'s exit status** — it exits 0 even when the datapath rejected the device.

### 2.5 Host firewall

**Ubuntu on OCI defaults to iptables, not nftables**, and its INPUT policy blocks the ports Spinifex serves. Open them explicitly and persist:

```bash
for p in 3000 8443 9999; do
    sudo iptables -I INPUT 1 -p tcp --dport "$p" -j ACCEPT
done
sudo netfilter-persistent save        # writes /etc/iptables/rules.v4
```

**Do not rely on appending.** Most distro rulesets end in a catch-all REJECT, so an appended ACCEPT lands behind it and does nothing. Insert at the head.

Spinifex installs its own rules and manages them itself; you do not need to create these by hand. For reference, a working node carries:

| Comment | Table/chain | Purpose |
| --- | --- | --- |
| `spinifex-imds` | `filter` INPUT | Accepts guest IMDS traffic arriving on any `ime-` endpoint |
| `spinifex-imds-remap` | `nat` PREROUTING | DNATs the guest-facing link-local pair to the host-side pair (§6) |
| `spinifex-eip-ingress` | `filter` FORWARD | Per-EIP forward accept, both directions |
| `spinifex-nat-egress` | `nat` POSTROUTING, `filter` FORWARD | Masquerade and conntrack accept for the transit subnet |

`uninstall-spx.sh` removes all four by comment marker.

---

## 3. Install the `oci` CLI

**The `oci` CLI is not on the Ubuntu image**, and `apt` has no `python3-oci-cli` package. Install it into a virtualenv:

```bash
sudo apt-get update && sudo apt-get install -y python3-venv
python3 -m venv ~/.oci-cli-venv
~/.oci-cli-venv/bin/pip install --upgrade pip oci-cli
sudo ln -sf ~/.oci-cli-venv/bin/oci /usr/local/bin/oci
oci --version                         # 3.94.0 at time of writing
```

**What the CLI is and is not for.** Spinifex's daemon uses the OCI **Go SDK** for every allocation — the CLI is not on that path. You need it for bootstrap (discovering OCIDs) and for operational inspection. Do not build automation that shells out to it during an allocation; allocations run inside a request budget that a forked Python process will not respect.

---

## 4. Credentials and IAM scopes

### 4.1 Which auth method

**Instance principal is the better design and is not what v1 uses.** The instance certificate is served at `/opc/v2/identity/cert.pem` and the Go SDK can authenticate as the instance with no key material on disk — but it still needs a dynamic group and a policy authorising it, and on the reference tenancy it authenticated without being authorised for anything. **v1 requires an operator-provisioned API key**, deliberately, to keep the setup a single documented path.

### 4.2 Create the user and key

In the OCI console, or with the CLI as an administrator:

```bash
# On your workstation, not the node.
openssl genrsa -out ~/.oci/oci_api_key.pem 2048
chmod 600 ~/.oci/oci_api_key.pem
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem \
  | openssl md5 -c        # this is the fingerprint
```

Upload the public key to the user under **Identity → Users → API Keys**.

### 4.3 The IAM policy

Spinifex calls exactly nine operations. Grant no more than these:

| Operation | Why |
| --- | --- |
| `CreatePrivateIp` | Register the secondary private IP the datapath carries |
| `DeletePrivateIp` | Release it |
| `GetPrivateIp` | Poll for `AVAILABLE` after an asynchronous assign |
| `ListPrivateIps` | The startup reconcile — find leaked objects |
| `CreatePublicIp` | Create the RESERVED public IP |
| `DeletePublicIp` | Release it |
| `GetPublicIp` | Poll lifecycle state |
| `GetPublicIpByPrivateIpId` | Reconcile a private IP back to its public half |
| `UpdatePublicIp` | Attach and detach the public IP from a private IP |

The matching policy, scoped to the one compartment Spinifex allocates in:

```
Allow group SpinifexOperators to manage private-ips in compartment <compartment-name>
Allow group SpinifexOperators to manage public-ips  in compartment <compartment-name>
Allow group SpinifexOperators to read   vnics       in compartment <compartment-name>
Allow group SpinifexOperators to read   subnets     in compartment <compartment-name>
Allow group SpinifexOperators to read   vcns        in compartment <compartment-name>
```

**Scope it to a compartment, not the tenancy.** `manage public-ips` at tenancy level lets a compromised node consume the whole regional reserved-public-IP quota.

**Do not grant `manage instances` or `manage vnics`.** Spinifex never creates, attaches or deletes a VNIC in v1 — it only adds addresses to a VNIC you already made. A grant that allows VNIC lifecycle is strictly more than the integration can use.

### 4.4 Quotas to check before you size a deployment

Two limits bound how many EIPs a node can hand out, and both are worth confirming against your tenancy rather than taking from documentation:

- **Reserved public IPs per region.** Default is 50. A cluster that hands out EIPs freely reaches this quickly, and a raise is a support ticket, not a setting.
- **Secondary private IPs per VNIC.** Current documentation says 64; older CLI reference says 32. The per-VNIC limit is the ceiling on one pool until multi-VNIC support lands.

### 4.5 Install the credentials on the node

**The daemon cannot read `~/.oci/`.** Its systemd unit sets `ProtectHome=yes`, so a config under any home directory is invisible to it however the permissions read. Credentials go under `/etc/spinifex/`:

```bash
sudo install -d -o root -g spinifex -m 0750 /etc/spinifex/oci
sudo install -o root -g spinifex -m 0640 ~/.oci/oci_api_key.pem /etc/spinifex/oci/oci_api_key.pem
sudo tee /etc/spinifex/oci/config >/dev/null <<'EOF'
[spinifex]
user=ocid1.user.oc1..aaaa...
fingerprint=aa:bb:cc:...
key_file=/etc/spinifex/oci/oci_api_key.pem
tenancy=ocid1.tenancy.oc1..aaaa...
region=ap-sydney-1
EOF
sudo chown root:spinifex /etc/spinifex/oci/config
sudo chmod 0640 /etc/spinifex/oci/config
```

Keep a copy at `~/.oci/config` as well — that is what the `oci` CLI reads for the bootstrap commands below, and it is a different consumer from the daemon.

Verify the key works before configuring Spinifex:

```bash
oci --config-file ~/.oci/config iam region list --query 'data[0].name' --raw-output
```

---

## 5. Install and configure Spinifex

### 5.1 Discover the OCIDs

```bash
# Instance, compartment and both VNICs, straight from the instance metadata.
curl -sH 'Authorization: Bearer Oracle' http://169.254.169.254/opc/v2/instance/ \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("compartment:", d["compartmentId"]); print("instance:", d["id"])'

curl -sH 'Authorization: Bearer Oracle' http://169.254.169.254/opc/v2/vnics/ \
  | python3 -c 'import json,sys
for v in json.load(sys.stdin):
    print(v["macAddr"], v["vnicId"], v["subnetCidrBlock"], v.get("privateIp"))'
```

**Pick the VNIC by MAC**, matching the MAC on the interface in your `br-wan` bridge. Oracle's VNIC ordering is not a documented contract, so "the second one" is not a selector.

The subnet OCID comes from the VNIC:

```bash
oci network vnic get --vnic-id <vnic-ocid> --query 'data."subnet-id"' --raw-output
```

### 5.2 Install

```bash
curl -sfL https://install.mulgadc.com | bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
```

**`--management` is load-bearing.** Omitting it takes the compute-node branch, which stops *and disables* `ovn-central` — silently, leaving a node with no record of what it was meant to be.

**`--nat-uplink`, not `--wan-bridge=br-wan`.** The two are mutually exclusive, and only `--nat-uplink` creates the `spx-nat` transit veth that routed mode runs on; `--wan-bridge` takes the veth-to-`br-ext` branch and deletes `spx-nat` on the way. `host.Routed.EnsureUplinkPort` then refuses with *"not on OVS — run setup-ovn.sh --nat-uplink"*. `br-wan` still exists and still holds the addresses — in routed mode nothing is bridged into `br-ext`, which is the whole point of it.

```bash
sudo spx admin init --node node1 --nodes 1 --region ap-southeast-2 \
    --external-mode=nat --ipsec=false
```

- **`--external-mode=nat` is mandatory** — see §1. `pool` mode produces a healthy-looking cluster that drops every guest packet.
- `--ipsec=false`: `openvswitch-ipsec` is typically masked on an OCI image while init defaults the flag to true.
- The Spinifex region is your own naming and has nothing to do with the OCI region. Do not read one as evidence about the other.

### 5.3 Configure the OCI pool

Add to `/etc/spinifex/spinifex.toml`:

```toml
[[network.external_pools]]
name               = "oci-public"
source             = "oci"
oci_compartment_id = "ocid1.compartment.oc1..aaaa..."
oci_vnic_id        = "ocid1.vnic.oc1.ap-sydney-1.abzx..."
oci_subnet_id      = "ocid1.subnet.oc1.ap-sydney-1.aaaa..."
oci_config_file    = "/etc/spinifex/oci/config"
oci_config_profile = "spinifex"
dns_servers        = ["169.254.169.253"]
```

Notes on the keys:

- **Exactly one of `oci_vnic_id` and `oci_vnic_iface`**, never both — the config is rejected if you set both or neither. Two that disagreed would send allocations to a VNIC the datapath is not on, and OCI would drop the traffic without a word. `oci_vnic_iface` resolves through instance metadata **by MAC**, so naming either `br-wan` or the physical interface resolves to the same VNIC.
- `oci_compartment_id` is the only other required key. `oci_subnet_id` is optional and defaults to the VNIC's own subnet; set it only when you want private IPs from a different subnet. `oci_config_file` defaults to `~/.oci/config` and `oci_config_profile` to `DEFAULT` — **on a node both need setting**, because the daemon cannot read a home directory (§4.5).
- `oci_public_ip_pool` takes a BYOIP pool OCID. Accepted today so BYOIP is a config change later rather than a code change; see §9.
- `range_start`, `range_end`, `gw_lrp_range_*`, `bind_bridge` and `dhcp_mac` are **rejected** on an OCI pool. OCI owns the addresses; a range you wrote would be fiction.
- Leave the `nat-transit` pool alone. In routed mode the per-VPC gateway router addresses come from the RFC 6598 transit range, not from OCI — **only EIPs consume an OCI address.** Fifty VPCs and three EIPs cost three reserved public IPs, not fifty-three.

### 5.4 Configure the IMDS host addresses

Required on OCI. See §6 for why.

```toml
[network]
imds_host_meta_ip = "169.254.42.254"
imds_host_dns_ip  = "169.254.42.253"
```

Then start:

```bash
sudo systemctl start spinifex.target
```

---

## 6. The `169.254.169.254` collision

Spinifex serves guest instance metadata on `169.254.169.254` and VPC DNS on `169.254.169.253`, because that is what an AWS-compatible guest expects. **OCI uses `169.254.169.254` for its own instance metadata**, and the host needs it — the identity certificate and the iSCSI boot path both depend on it.

Without the remap, the first guest launch puts `169.254.169.254/32` on a per-tap endpoint, `ip route get 169.254.169.254` starts returning `local … dev lo`, and the host's own metadata service stops answering.

**The guest keeps addressing `169.254.169.254`.** That is not negotiable — it is what AWS compatibility means. The remap is entirely host-side, and it is safe because the IMDS datapath is OpenFlow-steered rather than routed: the ARP responder, the ingress demux and the egress flow all match the guest-facing addresses and are untouched. Only two things change:

1. The per-tap `ime-` endpoint takes the host-side pair from config instead of the guest-facing pair.
2. A PREROUTING DNAT on `-i ime-+` translates the guest-facing pair to the host-side pair. Conntrack un-NATs the reply, so nothing on the return path needs to know.

Pick any unused link-local pair. `169.254.42.254` / `169.254.42.253` is the tested choice. **Set both keys or neither** — a half-configured pair is ignored, and the node behaves as if the remap were off.

Verify after the first guest launches:

```bash
ip route get 169.254.169.254                 # must be your real NIC, not lo
curl -sH 'Authorization: Bearer Oracle' -o /dev/null -w '%{http_code}\n' \
     http://169.254.169.254/opc/v2/instance/ # must be 200
ip -br addr show | grep ime-                 # endpoint holds 169.254.42.x, not .169.x
sudo iptables -t nat -S | grep spinifex-imds-remap
```

---

## 7. Verify end to end

```bash
export AWS_PROFILE=<your-profile>

aws ec2 allocate-address
aws ec2 run-instances --image-id <ami> --instance-type t3.micro --key-name <key> \
    --subnet-id <subnet> --associate-public-ip-address
aws ec2 describe-instances --query \
    'Reservations[].Instances[].[InstanceId,PrivateIpAddress,PublicIpAddress,State.Name]' \
    --output text
```

Then, from the host:

```bash
ping -c 3 <public-ip>
ssh -i <key> debian@<public-ip> hostname
```

And from inside the guest — note that IMDS is **IMDSv2, so it needs a token**; an untokened request correctly returns 401:

```bash
TOK=$(curl -s -X PUT http://169.254.169.254/latest/api/token \
      -H 'X-aws-ec2-metadata-token-ttl-seconds: 120')
curl -s -H "X-aws-ec2-metadata-token: $TOK" \
     http://169.254.169.254/latest/meta-data/instance-id
traceroute -n 8.8.8.8
```

A healthy topology is three hops before OCI's network: the OVN router port, the transit gateway, then upstream.

### What a correct node looks like

Worth knowing so the asymmetry does not read as a fault:

```bash
sudo ovn-nbctl lr-nat-list <logical-router>    # dnat_and_snat holds the PRIVATE address
aws ec2 describe-instances ...                 # reports the PUBLIC address
ip route show | grep spx-nat-host              # route to the PRIVATE address
```

**These disagreeing is the design.** The public address is 1:1-NATted by OCI upstream and never touches the wire, so every datapath object holds the private half while every AWS-facing record holds the public half. If OVN showed the public address, the rule would be matching something that never arrives.

---

## 8. Troubleshooting

**`RunInstances` returns `ServerInternal`.** Check the daemon journal for the real error — `journalctl -u spinifex-daemon --since -10m`. An OCI allocation failure surfaces this way.

**`InsufficientAddressCapacity` on `allocate-address`.** You have hit either the regional reserved-public-IP quota (default 50) or the per-VNIC secondary private IP limit. Check both; only the first can be raised by a ticket.

**Instance launches but the public address is unreachable.** In order:
1. `sudo ovn-nbctl lr-nat-list <router>` — is there a `dnat_and_snat` row, and does it hold the *private* address?
2. `ip route get <private-addr>` — does it route via the transit veth?
3. `sudo iptables -S FORWARD | grep spinifex-eip-ingress` — both directions present?
4. `oci network private-ip list --vnic-id <vnic>` — is the private address actually registered with OCI? If not, nothing else matters.

**Guests boot but cloud-init hangs on metadata.** Check §6 — this is the `169.254.169.254` collision. `ip route get 169.254.169.254` returning `dev lo` is the signature.

**The host loses its own metadata service after a guest terminates.** A known teardown leak: an `ime-` endpoint can survive guest termination still holding its address. With the remap configured this is harmless; without it, the host's metadata route is captured. Remove the stale OVS port by hand and configure the remap.

**Egress works from one address and not another on the same NIC.** That is §1's source-IP enforcement, and it means the address is not a registered private-IP object on the VNIC. It is never a routing problem on the host.

---

## 9. Limits in this version

- **Single node only.** Routed mode is capped at `--nodes=1`, and routed mode is the only viable OCI datapath.
- **Operator-provisioned API key**, not instance principal. The key is on the node's disk and its rotation is your responsibility.
- **One VNIC per pool.** A pool cannot grow past the per-VNIC secondary private IP limit.
- **IPv4 only.** IPv6 on OCI is materially simpler — no pools, prefixes assign directly to VCNs — but is not implemented.
- **No BYOIP.** The integration accepts a public IP pool OCID in config so BYOIP becomes a config change rather than a code change, but the RIR validation and Oracle's own validation window both precede any use of it.
- **No EIP failover between nodes**, which follows from single-node.
- **OCI Block Volumes are not an EBS provider.** They back `/var/lib/spinifex` and therefore viperblock, which captures most of the performance benefit, but there is no native OCI block provider.
