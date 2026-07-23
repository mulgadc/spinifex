package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/formation"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/mulgadc/viperblock/viperblock/v_utils"
	"github.com/nats-io/nats.go"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

//go:embed templates/spinifex.toml
var spinifexTomlTemplate string

//go:embed templates/awsgw.toml
var awsgwTomlTemplate string

//go:embed templates/predastore.toml
var predastoreTomlTemplate string

//go:embed templates/nats.conf
var natsConfTemplate string

//go:embed templates/predastore-multinode.toml
var predastoreMultiNodeTemplate string

//go:embed templates/northstar.toml
var northstarTomlTemplate string

var supportedArchs = map[string]bool{
	"x86_64":  true,
	"aarch64": true, // alias for arm64
	"arm64":   true,
}

// TODO: Confirm suppported platform types.
var supportedPlatforms = map[string]bool{
	"Linux/UNIX": true,
	"Windows":    true,
}

// Mirrors the gateway RegisterImage allowlist so admin imports can't write an
// AMI with a boot mode that RegisterImage would reject.
var supportedBootModes = map[string]bool{
	"bios":           true,
	"uefi":           true,
	"uefi-preferred": true,
}

var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Administrative commands for Spinifex platform management",
	Long:  `Administrative commands for initializing and managing the Spinifex platform.`,
}

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Cluster-wide operations",
	Long:  `Cluster-wide administrative operations such as coordinated shutdown.`,
}

var clusterShutdownCmd = &cobra.Command{
	Use:   "shutdown",
	Short: "Gracefully shut down the entire cluster",
	Long: `Perform a coordinated, phased shutdown of the entire cluster.
Phases execute in order: GATE (stop API/UI) → DRAIN (stop VMs) → STORAGE (stop viperblock) → PERSIST (stop predastore) → INFRA (stop NATS/daemon).
Each phase waits for all nodes to ACK before proceeding to the next.`,
	Run: runClusterShutdown,
}

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Node-local operations",
	Long:  `Node-local administrative operations such as a graceful local guest drain.`,
}

var nodeDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Gracefully drain guests on the local node",
	Long: `Run the GATE and DRAIN shutdown phases against the local node only: power down
its guests via QMP and unmount their volumes (flushing the viperblock WAL) while
every service is still running. STORAGE/PERSIST/INFRA are left to systemd's
ordered unit teardown. This is the ExecStop of spinifex-shutdown.service, so a
systemctl stop or host reboot drains guests before any storage service stops.`,
	Run: runNodeDrainLocal,
}

var clusterDrainDHCPCmd = &cobra.Command{
	Use:   "drain-dhcp",
	Short: "Release all upstream DHCP leases held by vpcd",
	Long: `Ask each vpcd to DHCPRELEASE every external-pool DHCP lease it currently
holds, returning them to the upstream DHCP server. Run this on teardown before
stopping services: an env reset otherwise strands held leases on the upstream
server until their TTL expires, eventually exhausting the upstream scope.`,
	Run: runClusterDrainDHCP,
}

var adminInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize Spinifex platform configuration",
	Long: `Initialize Spinifex platform by creating configuration files, generating SSL certificates,
and setting up AWS credentials. This creates the necessary directory structure and
configuration files in ~/spinifex/config.`,
	Run: runAdminInit,
}

var adminJoinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join an existing Spinifex cluster",
	Long: `Join an existing Spinifex cluster by connecting to a leader node and retrieving
the cluster configuration. This command will configure the local node to join
the cluster and participate in distributed operations.`,
	Run: runAdminJoin,
}

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Manage OS images",
	Long:  `Manage OS images for local storage and AMI creation.`,
}

var imagesImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Specify local file to import",
	Long:  `Create a new image from a local file`,
	Run:   runimagesImportCmd,
}

var imagesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List OS images to import or download",
	Long:  `Query the remote endpoint for common OS images available for import as AMI or locally download.`,
	Run:   runimagesListCmd,
}

var imagesRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an admin-imported system AMI",
	Long: `Delete an AMI imported via 'spx admin images import', including its
backing block storage and snapshot artefacts. Only operates on AMIs with a
non-account owner (e.g. "system"); account-owned AMIs must be removed via
'aws ec2 deregister-image' followed by 'aws ec2 delete-snapshot'.

Refuses to delete an AMI that has dependent volumes or copied snapshots/AMIs
unless --force is passed. Prompts for confirmation unless --yes is passed.`,
	Run: runimagesRemoveCmd,
}

var imagesPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Promote an account-owned AMI to a system image",
	Long: `Rewrite an account-owned AMI's owner to the system alias so it becomes
visible to all accounts via DescribeImages, matching the behaviour of AMIs
imported via 'spx admin images import'.

No data is copied — only the config.json owner field is updated. The change
takes effect immediately. Prompts for confirmation unless --yes is passed.`,
	Run: runimagesPromoteCmd,
}

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage Spinifex accounts",
	Long:  `Create and manage Spinifex accounts. Each account namespaces IAM users, policies, and resources.`,
}

var accountCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new account with an admin user",
	Long: `Create a new Spinifex account. This creates an account with a sequential 12-digit ID,
an admin user, and an AdministratorAccess policy attached to the admin user.
Requires the cluster to be running (connects to NATS).`,
	Run: runAccountCreate,
}

var accountListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all accounts",
	Long:  `List all Spinifex accounts with their ID, name, status, and creation time.`,
	Run:   runAccountList,
}

var adminBannerCmd = &cobra.Command{
	Use:   "banner",
	Short: "Write the Spinifex console banner to /etc/issue and /etc/motd",
	Long: `Writes the node information banner to /etc/issue (shown before login on
physical/serial console) and /etc/motd (shown after SSH login).

With --boot-check, also detects if the management IP has changed since last
boot and updates /etc/spinifex/node.conf accordingly.`,
	Run: runAdminBanner,
}

var certCmd = &cobra.Command{
	Use:   "cert",
	Short: "Manage TLS certificates",
	Long:  `Manage TLS certificates for the Spinifex platform.`,
}

var certRenewCmd = &cobra.Command{
	Use:   "renew",
	Short: "Regenerate the server certificate with current IPs",
	Long: `Regenerate the server certificate signed by the existing CA.
The new certificate will include all current network interface IPs
and the machine hostname in its SANs. Use this after adding a new
network interface or changing IP addresses.`,
	Run: runCertRenew,
}

/*
CLI ideas

spx admin images list

- fetches from remote endpoint for common/trusted images to bootstrap environment, or baked in from compile.

// If --name specified, download
spx admin images import --name debian-13-x86_64

// List available images
spx admin images list

// Manually import a path
spx admin images import --file /path/to/image --distro debian --version 13 --arch x86_64

-> x <-
*/

func init() {
	rootCmd.AddCommand(adminCmd)
	adminCmd.AddCommand(adminInitCmd)
	adminCmd.AddCommand(adminJoinCmd)

	adminCmd.AddCommand(clusterCmd)
	clusterCmd.AddCommand(clusterShutdownCmd)
	clusterShutdownCmd.Flags().Bool("force", false, "Force shutdown even if nodes don't respond")
	clusterShutdownCmd.Flags().Duration("timeout", 120*time.Second, "Maximum time to wait per phase")
	clusterShutdownCmd.Flags().Bool("dry-run", false, "Print phase plan without executing")

	clusterCmd.AddCommand(clusterDrainDHCPCmd)
	clusterDrainDHCPCmd.Flags().Duration("timeout", 30*time.Second, "Reply-collection window for vpcd drain responders")

	adminCmd.AddCommand(nodeCmd)
	nodeCmd.AddCommand(nodeDrainCmd)
	nodeDrainCmd.Flags().Bool("local", false, "Drain the local node only (required)")
	nodeDrainCmd.Flags().Duration("timeout", 120*time.Second, "Maximum time to wait per phase")

	adminCmd.AddCommand(imagesCmd)
	imagesCmd.AddCommand(imagesImportCmd)
	imagesCmd.AddCommand(imagesListCmd)
	imagesCmd.AddCommand(imagesRemoveCmd)
	imagesCmd.AddCommand(imagesPromoteCmd)

	adminCmd.AddCommand(accountCmd)
	accountCmd.AddCommand(accountCreateCmd)
	accountCmd.AddCommand(accountListCmd)

	adminCmd.AddCommand(adminBannerCmd)
	adminBannerCmd.Flags().Bool("boot-check", false, "Check for management IP change and update node.conf if needed")

	adminCmd.AddCommand(certCmd)
	certCmd.AddCommand(certRenewCmd)

	adminCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().Bool("yes", false, "Apply migrations without prompting")
	certRenewCmd.Flags().StringSlice("extra-ip", nil, "Additional IP addresses to include in SANs")
	certRenewCmd.Flags().StringSlice("extra-dns", nil, "Additional DNS names to include in SANs")
	accountCreateCmd.Flags().String("name", "", "Account name (required)")
	accountCreateCmd.MarkFlagRequired("name")

	rootCmd.PersistentFlags().String("config-dir", DefaultConfigDir(), "Configuration directory")
	rootCmd.PersistentFlags().String("spinifex-dir", DefaultDataDir(), "Spinifex base directory")

	// Flags for admin init
	adminInitCmd.Flags().Bool("force", false, "Force re-initialization (overwrites existing config)")
	adminInitCmd.Flags().String("region", "ap-southeast-2", "Mulga region to create")
	adminInitCmd.Flags().String("az", "ap-southeast-2a", "Mulga AZ to create")
	adminInitCmd.Flags().String("node", "node1", "Node name, increment for additional nodes (default, node1)")
	adminInitCmd.Flags().Int("nodes", 3, "Number of nodes to expect for cluster")
	adminInitCmd.Flags().String("host", "", "Leader node to join (if not specified, tries multicast discovery)")
	adminInitCmd.Flags().Int("port", 4432, "Port to bind cluster services on")
	adminInitCmd.Flags().String("bind", "0.0.0.0", "IP address to bind services to (e.g., 10.11.12.1 for multi-node). Default 0.0.0.0 listens on all interfaces.")
	adminInitCmd.Flags().String("advertise", "", "External IP that off-host clients (ALB VMs, remote operators) should dial. Auto-detected from default route when empty.")
	adminInitCmd.Flags().String("cluster-bind", "", "IP address to bind NATS cluster services to (e.g., 10.11.12.1 for multi-node)")
	adminInitCmd.Flags().String("cluster-routes", "", "NATS cluster hosts for routing specify multiple with comma (e.g., 10.11.12.1:4248,10.11.12.2:4248 for multi-node)")
	adminInitCmd.Flags().String("predastore-nodes", "", "Comma-separated IPs for multi-node Predastore cluster (e.g., 10.11.12.1,10.11.12.2,10.11.12.3). Requires >= 3 nodes.")
	adminInitCmd.Flags().String("formation-timeout", "10m", "Timeout for cluster formation (e.g., 5m, 30s)")
	adminInitCmd.Flags().String("token-ttl", "30m", "Join token validity duration (e.g. 30m, 1h, 2h)")
	adminInitCmd.Flags().Int("predastore-compaction-interval", 0, "Predastore compactor interval in seconds (0 = unset, uses built-in default). Test clusters set a short interval.")
	adminInitCmd.Flags().String("cluster-name", "spinifex", "NATS cluster name")
	adminInitCmd.Flags().Bool("no-telemetry", false, "Disable telemetry metrics sent during init (default: enabled)")
	adminInitCmd.Flags().String("email", "", "Operator email address (used for update and security notifications)")
	adminInitCmd.Flags().StringSlice("services", nil, "Services this node runs (default: all). Valid: nats,predastore,viperblock,daemon,awsgw,ui")

	// External networking flags
	adminInitCmd.Flags().String("external-mode", "", "External network mode: 'pool' (default when WAN detected), 'nat' (routed; non-bridgeable uplinks; add --external-pool or --external-source=dhcp for public IPs), or '' (disabled)")
	adminInitCmd.Flags().String("external-iface", "", "WAN NIC for br-external (auto-detected from default route)")
	adminInitCmd.Flags().String("external-source", "", "Pool IP source: 'dhcp' (default when no --external-pool) or 'static' (uses --external-pool range)")
	adminInitCmd.Flags().String("external-bind-bridge", "", "Linux bridge for upstream DHCP DORA (default 'br-wan' when --external-source=dhcp)")
	adminInitCmd.Flags().String("external-pool", "", "External IP pool range as start-end (e.g., 192.168.1.150-192.168.1.250)")
	adminInitCmd.Flags().String("external-gateway", "", "WAN gateway IP (auto-detected from default route)")
	adminInitCmd.Flags().String("gateway-ip", "", "OVN gateway router's external IP for SNAT (default: pool range_start for pool mode, required for nat mode without DHCP)")
	adminInitCmd.Flags().Int("external-prefix-len", 24, "External pool subnet prefix length (auto-detected)")
	adminInitCmd.Flags().Bool("gpu-passthrough", false, "Enable VFIO GPU passthrough (sets gpu_passthrough = true in daemon config)")
	adminInitCmd.Flags().Bool("ipsec", true, "Encrypt intra-AZ Geneve via OVN native IPsec (cluster-wide); disable only for trusted single-rack lab")

	// Flags for admin join
	adminJoinCmd.Flags().String("region", "ap-southeast-2", "Region for this node")
	adminJoinCmd.Flags().String("az", "ap-southeast-2a", "Availability zone for this node")
	adminJoinCmd.Flags().String("node", "", "Node name (required)")
	adminJoinCmd.Flags().String("host", "", "Leader node host:port (e.g., node1.local:4432) (required)")
	adminJoinCmd.Flags().String("data-dir", "", "Data directory for this node (default: ~/spinifex)")
	adminJoinCmd.Flags().Int("port", 4432, "Port to bind cluster services on")
	adminJoinCmd.Flags().String("bind", "0.0.0.0", "IP address to bind services to (e.g., 10.11.12.2 for multi-node on single host)")
	adminJoinCmd.Flags().String("advertise", "", "External IP this node advertises to other cluster members. Defaults to --bind, or auto-detected WAN IP when --bind is 0.0.0.0.")
	adminJoinCmd.Flags().String("cluster-bind", "", "IP address to bind NATS cluster services to (e.g., 10.11.12.1 for multi-node)")
	adminJoinCmd.Flags().String("cluster-routes", "", "NATS cluster hosts for routing specify multiple with comma (e.g., 10.11.12.1:4248,10.11.12.2:4248 for multi-node)")
	adminJoinCmd.Flags().String("token", "", "Join token from the init node (required)")
	adminJoinCmd.Flags().StringSlice("services", nil, "Services this node runs (default: all)")
	adminJoinCmd.Flags().Bool("no-telemetry", false, "Disable telemetry metrics sent during join (default: enabled)")
	adminJoinCmd.Flags().String("email", "", "Operator email address (used for update and security notifications)")
	adminJoinCmd.Flags().Int("predastore-compaction-interval", 0, "Predastore compactor interval in seconds (0 = unset, uses built-in default). Test clusters set a short interval.")
	adminJoinCmd.MarkFlagRequired("node")
	adminJoinCmd.MarkFlagRequired("host")
	adminJoinCmd.MarkFlagRequired("token")

	imagesImportCmd.Flags().String("tmp-dir", os.TempDir(), "Temporary directory for image import processing")

	imagesImportCmd.Flags().String("name", "", "Import specified image by name")
	imagesImportCmd.Flags().String("ami-name", "", "Override the registered AMI name (DescribeImages name). Defaults to ami-{distro}-{version}-{arch}. Use for locally-built appliances (e.g. --ami-name spinifex-eks-node).")
	imagesImportCmd.Flags().String("file", "", "Import file from specified path (raw, qcow2, compressed)")
	imagesImportCmd.Flags().String("distro", "", "Specified distro name (e.g debian)")
	imagesImportCmd.Flags().String("version", "", "Specified distro version (e.g 12)")
	imagesImportCmd.Flags().String("arch", "", "Specified distro arch (e.g aarch64, arm64, x86_64)")
	imagesImportCmd.Flags().String("platform", "Linux/UNIX", "Specified platform (e.g Linux/UNIX, Windows)")
	imagesImportCmd.Flags().String("boot-mode", "", "Boot mode for the imported AMI (bios|uefi|uefi-preferred). Required with --file. Overrides the catalog value when used with --name.")
	imagesImportCmd.Flags().StringSlice("tag", nil, "Tag to apply to the imported AMI as key=value (repeatable; e.g. --tag spinifex:managed-by=elbv2)")
	imagesImportCmd.Flags().Bool("force", false, "Force command execution (overwrites existing files)")
	imagesImportCmd.Flags().Bool("skip-verify", false, "Skip catalog-image checksum verification (INSECURE; operator assumes integrity responsibility)")

	imagesRemoveCmd.Flags().String("image-id", "", "AMI ID to remove (required)")
	imagesRemoveCmd.Flags().Bool("force", false, "Bypass dependency, ownership and config-corrupt checks (salvage mode)")
	imagesRemoveCmd.Flags().Bool("yes", false, "Skip interactive confirmation prompt")
	if err := imagesRemoveCmd.MarkFlagRequired("image-id"); err != nil {
		panic(err)
	}

	imagesPromoteCmd.Flags().String("image-id", "", "AMI ID to promote to system image (required)")
	imagesPromoteCmd.Flags().Bool("yes", false, "Skip interactive confirmation prompt")
	if err := imagesPromoteCmd.MarkFlagRequired("image-id"); err != nil {
		panic(err)
	}
}

// amiVolumeSizeGiB returns the smallest whole GiB that still holds sizeBytes.
//
// Rounding up is load-bearing. The image is copied into a root volume of
// exactly this size, so a volume smaller than the image truncates it and the
// guest comes up with no root partition — it stalls on the root device until
// systemd drops it to an emergency shell, and nothing on the way there reports
// an undersized volume. Flooring (plain integer division) undersizes every
// image that is not an exact multiple of a GiB, which is why this went unseen
// while every system image happened to be a round 16 GiB.
//
// The downstream guard cannot cover for a wrong answer here: floorVolumeSizeToAMI
// raises a caller's requested size to this value, so it inherits the mistake
// rather than catching it.
func amiVolumeSizeGiB(sizeBytes int64) uint64 {
	const bytesPerGiB = 1024 * 1024 * 1024
	if sizeBytes <= 0 {
		return 0
	}
	return utils.SafeInt64ToUint64((sizeBytes + bytesPerGiB - 1) / bytesPerGiB)
}

func runimagesImportCmd(cmd *cobra.Command, args []string) {
	var image utils.Images

	var imageFile string
	var imageStat os.FileInfo
	var err error

	cfgFile, _ := cmd.Flags().GetString("config")
	forceCmd, _ := cmd.Flags().GetBool("force")
	skipVerify, _ := cmd.Flags().GetBool("skip-verify")
	ostmpDir, _ := cmd.Flags().GetString("tmp-dir")

	// Use default config path
	if cfgFile == "" {
		cfgFile = DefaultConfigFile()
	}

	//configDir, _ := cmd.Flags().GetString("config-dir")
	baseDir, _ := cmd.Flags().GetString("spinifex-dir")

	// Strip trailing slash
	baseDir = filepath.Clean(baseDir)

	// Check the base dir has our images path, and correctlty init
	imageDir := fmt.Sprintf("%s/images", baseDir)

	if !admin.FileExists(imageDir) {
		fmt.Fprintf(os.Stderr, "Error: image directory does not exist: %s\n\n", imageDir)
		fmt.Fprintf(os.Stderr, "Run 'spx admin init' first to initialize the Spinifex platform.\n")
		os.Exit(1)
	}

	// --name pulls metadata (including Tags) from the catalog; --file supplies
	// the local image source. When both are set, the catalog provides metadata
	// and the file is used directly (no download). When only --file is set,
	// metadata comes from flags and no catalog tags are applied.
	imageName, _ := cmd.Flags().GetString("name")
	amiNameOverride, _ := cmd.Flags().GetString("ami-name")
	localFile, _ := cmd.Flags().GetString("file")

	if imageName == "" && localFile == "" {
		fmt.Fprintf(os.Stderr, "Either --name or --file is required to import an image\n")
		os.Exit(1)
	}

	if imageName != "" {
		var exists bool
		image, exists = utils.AvailableImages[imageName]
		if !exists {
			fmt.Fprintf(os.Stderr, "Image name not found in available images")
			os.Exit(1)
		}
	}

	if localFile != "" {
		if _, err := os.Stat(localFile); err != nil {
			fmt.Fprintf(os.Stderr, "File could not be found: %s", err)
			os.Exit(1)
		}
		imageFile = localFile
		if imageName == "" {
			image.Distro, _ = cmd.Flags().GetString("distro")
			image.Version, _ = cmd.Flags().GetString("version")
			image.Arch, _ = cmd.Flags().GetString("arch")
			image.Platform, _ = cmd.Flags().GetString("platform")
		}
	}

	// --file imports have no catalog metadata to inherit from, so the operator
	// must declare the boot mode explicitly — guessing would silently produce
	// a BIOS AMI from a UEFI-only image (or vice versa) and fail at launch.
	// --name imports inherit from the catalog; the flag overrides when set.
	bootModeFlag, _ := cmd.Flags().GetString("boot-mode")
	if bootModeFlag != "" {
		if !supportedBootModes[bootModeFlag] {
			fmt.Fprintf(os.Stderr, "Unsupported --boot-mode %q (expected bios|uefi|uefi-preferred)\n", bootModeFlag)
			os.Exit(1)
		}
		image.BootMode = bootModeFlag
	} else if imageName == "" {
		fmt.Fprintf(os.Stderr, "--boot-mode is required when importing via --file (expected bios|uefi|uefi-preferred)\n")
		os.Exit(1)
	}

	// --tag k=v (repeatable) merges user-supplied tags into the AMI. Overrides
	// catalog tags on key collision so operators can re-tag a known image.
	tagFlags, _ := cmd.Flags().GetStringSlice("tag")
	for _, kv := range tagFlags {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			fmt.Fprintf(os.Stderr, "Invalid --tag %q: expected key=value\n", kv)
			os.Exit(1)
		}
		if image.Tags == nil {
			image.Tags = map[string]string{}
		}
		image.Tags[k] = v
	}

	if image.Distro == "" {
		fmt.Fprintf(os.Stderr, "Specify distro name")
		os.Exit(1)
	}

	// Check version specified
	if image.Version == "" {
		fmt.Fprintf(os.Stderr, "Specify image version")
		os.Exit(1)
	}

	if !supportedArchs[image.Arch] {
		fmt.Fprintf(os.Stderr, "Unsupported architecture")
		os.Exit(1)
	}

	if !supportedPlatforms[image.Platform] {
		fmt.Fprintf(os.Stderr, "Unsupported platform")
		os.Exit(1)
	}

	// Create the specified image directory
	imagePath := fmt.Sprintf("%s/%s/%s/%s", imageDir, image.Distro, image.Version, image.Arch)

	// Create config directory
	if err := os.MkdirAll(imagePath, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config directory: %v\n", err)
		os.Exit(1)
	}

	// Next, if the file is selected to download, fetch it, extract disk image, and save to path
	if imageName != "" && localFile == "" {
		// Download the file to the image path
		filename := path.Base(image.URL)
		imageFile = fmt.Sprintf("%s/%s", imagePath, filename)

		// If image path exists, skip
		if admin.FileExists(imageFile) && !forceCmd {
			fmt.Printf("Image file already exists, skipping download, use --force to overwrite: %s\n", imageFile)
		} else {
			err := utils.DownloadFileWithProgress(image.URL, image.Name, imageFile, 0)

			if err != nil {
				fmt.Printf("Download failed: %v\n", err)
				os.Exit(1)
			}
		}

		// Verify before extract: the catalog digest is of the artifact as it
		// sits on the mirror (.tar.xz/.img/.raw). Also catches a poisoned
		// cache on the re-run path — failure leaves the file on disk so the
		// operator can inspect; recover with --force.
		if skipVerify {
			fmt.Fprintf(os.Stderr, "⚠️  --skip-verify set: checksum verification skipped for %s\n", imageName)
		} else {
			if image.Checksum == "" || image.ChecksumType == "" {
				fmt.Fprintf(os.Stderr, "Catalog entry %q is missing Checksum/ChecksumType; refusing import.\n", imageName)
				os.Exit(1)
			}
			if err := utils.VerifyImageChecksum(imageFile, image.Checksum, image.ChecksumType); err != nil {
				printChecksumError(os.Stderr, imageFile, imageName, image, err)
				os.Exit(1)
			}
			fmt.Printf("✅ Verified image checksum (%s)\n", image.ChecksumType)
		}
	}

	// Next, validate if the image is raw, tar, gz, xv, etc. We need to upload the raw image
	tmpDir, err := os.MkdirTemp(ostmpDir, "spinifex-image-tmp-*")

	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not create temp dir: %v\n", err)
		os.Exit(1)
	}

	extractedImagePath, err := utils.ExtractDiskImageFromFile(imageFile, imagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not extract image: %v\n", err)
		os.Exit(1)
	}

	imageStat, err = os.Stat(extractedImagePath)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not stat image: %v\n", err)
		os.Exit(1)
	}

	// Create the specified manifest file to describe the image/AMI
	manifest := viperblock.VolumeConfig{}

	// Calculate the size

	manifest.AMIMetadata.Name = fmt.Sprintf("ami-%s-%s-%s", image.Distro, image.Version, image.Arch)
	if amiNameOverride != "" {
		manifest.AMIMetadata.Name = amiNameOverride
	}
	volumeId := utils.GenerateResourceID("ami")
	manifest.AMIMetadata.ImageID = volumeId

	manifest.AMIMetadata.Description = fmt.Sprintf("%s cloud image prepared for Spinifex", manifest.AMIMetadata.Name)
	manifest.AMIMetadata.Architecture = image.Arch
	manifest.AMIMetadata.PlatformDetails = image.Platform
	manifest.AMIMetadata.CreationDate = time.Now()
	manifest.AMIMetadata.RootDeviceType = "ebs"
	manifest.AMIMetadata.Virtualization = "hvm"
	manifest.AMIMetadata.ImageOwnerAlias = "system"
	manifest.AMIMetadata.VolumeSizeGiB = amiVolumeSizeGiB(imageStat.Size())
	manifest.AMIMetadata.BootMode = image.BootMode
	manifest.AMIMetadata.Distro = image.Distro
	manifest.AMIMetadata.DistroFamily = utils.DistroFamily(image.Distro)

	// Copy catalog-provided tags (e.g. spinifex:managed-by for system AMIs)
	// onto the imported AMI so the UI can filter them out.
	if len(image.Tags) > 0 {
		manifest.AMIMetadata.Tags = make(map[string]string, len(image.Tags))
		maps.Copy(manifest.AMIMetadata.Tags, image.Tags)
	}

	// Volume Data
	manifest.VolumeMetadata.VolumeID = volumeId // TODO: Confirm if unique, e.g vol-, if ami- used
	manifest.VolumeMetadata.VolumeName = manifest.AMIMetadata.Name
	manifest.VolumeMetadata.TenantID = "system"
	manifest.VolumeMetadata.SizeGiB = manifest.AMIMetadata.VolumeSizeGiB
	manifest.VolumeMetadata.State = "available"
	manifest.VolumeMetadata.AvailabilityZone = "" // TODO: Confirm
	manifest.VolumeMetadata.CreatedAt = time.Now()
	manifest.VolumeMetadata.VolumeType = "gp3"
	manifest.VolumeMetadata.IOPS = 1000

	// Write the manifest to disk
	// Save as JSON
	jsonData, err := json.Marshal(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not marshal manifest: %v\n", err)
		os.Exit(1)
	}

	manifestFilename := fmt.Sprintf("%s/%s.json", imagePath, manifest.AMIMetadata.Name)
	// Write to file
	err = os.WriteFile(manifestFilename, jsonData, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not write manifest: %v\n", err)
		os.Exit(1)
	}

	// Upload the image to S3 (predastore)

	appConfig, err := config.LoadConfig(cfgFile)

	if err != nil {
		fmt.Println("Error loading config file:", err)
		return
	}

	s3Config := s3.S3Config{
		VolumeName: volumeId,
		VolumeSize: utils.SafeInt64ToUint64(imageStat.Size()),
		Bucket:     appConfig.Nodes[appConfig.Node].Predastore.Bucket,
		Region:     appConfig.Nodes[appConfig.Node].Predastore.Region,
		AccessKey:  appConfig.Nodes[appConfig.Node].Predastore.AccessKey,
		SecretKey:  appConfig.Nodes[appConfig.Node].Predastore.SecretKey,
		Host:       appConfig.Nodes[appConfig.Node].Predastore.Host,
	}

	mkey, err := utils.LoadViperblockMasterKey(appConfig.Nodes[appConfig.Node].Viperblock.EncryptionKeyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not load viperblock encryption key: %v\n", err)
		os.Exit(1)
	}

	// Import is an interactive command whose output is a progress bar, so the
	// volume's routine mount chatter is noise here: a fresh import has no prior
	// state, making the "no state found" / 404 lines expected rather than
	// notable. Scoping the logger to this VB (New copies it onto the backend
	// too) keeps it off the process-wide default. Errors still surface, both
	// through this logger and as the returned error the caller prints.
	vbConfig := viperblock.VB{
		VolumeName: volumeId,
		VolumeSize: utils.SafeInt64ToUint64(imageStat.Size()),
		BaseDir:    tmpDir,
		Cache: viperblock.Cache{
			Config: viperblock.CacheConfig{
				Size: 0,
			},
		},
		VolumeConfig:      manifest,
		MasterKey:         mkey,
		EncryptionEnabled: mkey != nil,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	// Bake the deployment CA into the image trust store so a stock cloud image
	// trusts the gateway from first boot. Best-effort: never fails the import.
	// The CA path derives from the --spinifex-dir data root (its config/ symlink
	// holds ca.pem), not NodeBaseDir(): node.BaseDir is unset in the import
	// command, which would collapse the path to a relative config/ca.pem.
	bakeCACertIntoImage(extractedImagePath, filepath.Join(baseDir, "config", "ca.pem"))

	// Render the flush bar here rather than inside viperblock, which stays a
	// pure storage library. The callback is created lazily on the first update
	// so the total (image size in bytes) is known, and viperblock throttles it
	// to ≤101 invocations — so the bar renders human-readable sizes in its title
	// without the render-frequency regression a per-block update would cause.
	var flushBar *pterm.ProgressbarPrinter
	var flushTotalHuman string
	progress := func(current, total uint64) {
		if flushBar == nil {
			flushTotalHuman = utils.HumanBytes(total)
			// pterm's elapsed-time display normally spawns a background goroutine
			// that re-renders every second with no lock, interleaving with our
			// own low-frequency renders and tearing the line. Start with it off
			// so no timer is spawned, then flip the flag on — pterm still appends
			// its own elapsed time, now emitted only on our (single) renders.
			flushBar, _ = pterm.DefaultProgressbar.
				WithTitle("Flushing image to storage").
				WithTotal(utils.SafeUint64ToInt(total)).
				WithShowCount(false).       // hide raw ints; the size goes in the title
				WithShowElapsedTime(false). // suppress the async re-render (see above)
				Start()
			flushBar.ShowElapsedTime = true // keep pterm's elapsed, rendered only by us
		}
		flushBar.Current = utils.SafeUint64ToInt(current) // drives fill + percentage
		// UpdateTitle performs the single render for this step.
		flushBar.UpdateTitle(fmt.Sprintf("Flushing image to storage — %s / %s", utils.HumanBytes(current), flushTotalHuman))
	}

	err = v_utils.ImportDiskImage(&s3Config, &vbConfig, extractedImagePath, progress)

	if flushBar != nil {
		_, _ = flushBar.Stop()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not import image to predastore: %v\n", err)
		os.Exit(1)
	}

	defer os.RemoveAll(tmpDir)

	fmt.Printf("✅ Image import complete. Image-ID (AMI): %s\n", volumeId)
}

// caBakeRunCommand installs the uploaded CA into the guest trust store across
// the distro families, then removes the staged copy. Each branch is gated on its
// updater existing so only the matching distro's branch writes anything (install
// -D creates the anchor dir on minimal images). Debian/Ubuntu use
// update-ca-certificates, RHEL/Rocky use update-ca-trust, and stock Alpine ships
// only ca-certificates-bundle (no updater, no anchor dir) so it falls back to
// appending the PEM to the static bundle. The install chain's exit code is
// preserved past the cleanup rm so a failure surfaces instead of a false success.
// The RHEL branch restorecon's only the trust-store paths it touched so the cert
// is correctly labelled without the image-wide --no-selinux-relabel (below)
// leaving an unlabelled bundle; restorecon is best-effort and never fails the bake.
const caBakeRunCommand = `( { command -v update-ca-certificates >/dev/null && install -D -m644 /tmp/spinifex-ca.pem /usr/local/share/ca-certificates/spinifex.crt && update-ca-certificates; } || ` +
	`{ command -v update-ca-trust >/dev/null && install -D -m644 /tmp/spinifex-ca.pem /etc/pki/ca-trust/source/anchors/spinifex.crt && update-ca-trust && ` +
	`{ command -v restorecon >/dev/null 2>&1 && restorecon -RF /etc/pki/ca-trust/source/anchors/spinifex.crt /etc/pki/ca-trust/extracted >/dev/null 2>&1; true; }; } || ` +
	`{ cat /tmp/spinifex-ca.pem >> /etc/ssl/certs/ca-certificates.crt; } ); ` +
	`rc=$?; rm -f /tmp/spinifex-ca.pem; exit $rc`

// caBakeTimeout bounds the virt-customize run so a stalled libguestfs appliance
// cannot hang the import indefinitely; on timeout the bake degrades to a skip.
const caBakeTimeout = 5 * time.Minute

// caBakeCmd builds the virt-customize invocation that uploads the deployment CA
// into the disk image at imagePath and installs it into the guest trust store.
// --no-selinux-relabel stops virt-customize flagging the image for a first-boot
// SELinux autorelabel: that relabel+reboot corrupts XFS roots (RHEL/Rocky) on the
// reboot, so the run-command relabels only the touched trust paths instead.
func caBakeCmd(ctx context.Context, imagePath, caCertPath string) *exec.Cmd {
	return exec.CommandContext(ctx, "virt-customize", "-a", imagePath,
		"--no-selinux-relabel",
		"--upload", caCertPath+":/tmp/spinifex-ca.pem",
		"--run-command", caBakeRunCommand)
}

// caBakeRunner resolves virt-customize and runs the CA bake; overridable in tests.
var caBakeRunner = func(imagePath, caCertPath string) ([]byte, error) {
	if _, err := exec.LookPath("virt-customize"); err != nil {
		return nil, fmt.Errorf("virt-customize not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), caBakeTimeout)
	defer cancel()
	return caBakeCmd(ctx, imagePath, caCertPath).CombinedOutput()
}

// bakeCACertIntoImage uploads the deployment CA into the image's trust store via
// virt-customize so an imported stock image trusts the gateway from first boot.
// Best-effort: a missing CA, an absent virt-customize, or an image libguestfs
// cannot inspect logs and continues — image import never fails on the CA.
func bakeCACertIntoImage(imagePath, caCertPath string) {
	if _, err := os.Stat(caCertPath); err != nil {
		slog.Warn("CA bake skipped: deployment CA not found; imported image will not auto-trust the gateway", "ca", caCertPath, "err", err)
		return
	}
	if out, err := caBakeRunner(imagePath, caCertPath); err != nil {
		slog.Warn("CA bake skipped: virt-customize could not customize image; imported image will not auto-trust the gateway", "err", err, "output", string(out))
		return
	}
}

func runimagesRemoveCmd(cmd *cobra.Command, args []string) {
	imageID, _ := cmd.Flags().GetString("image-id")
	force, _ := cmd.Flags().GetBool("force")
	yes, _ := cmd.Flags().GetBool("yes")

	cfgFile, _ := cmd.Flags().GetString("config")
	if cfgFile == "" {
		cfgFile = DefaultConfigFile()
	}

	appConfig, err := config.LoadConfig(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error loading config file:", err)
		os.Exit(1)
	}

	node := appConfig.Nodes[appConfig.Node]
	store := objectstore.NewS3ObjectStoreFromConfig(
		node.Predastore.Host,
		node.Predastore.Region,
		node.Predastore.AccessKey,
		node.Predastore.SecretKey,
	)
	bucket := node.Predastore.Bucket

	preview, err := admin.PreviewRemoveSystemImage(store, bucket, imageID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to inspect AMI:", err)
		os.Exit(1)
	}

	// Print metadata block.
	fmt.Println("About to remove system AMI:")
	fmt.Println()
	fmt.Printf("  Image ID:        %s\n", preview.ImageID)
	switch {
	case !preview.ConfigPresent && !preview.ConfigCorrupt:
		fmt.Println("  Name:            <unknown — config.json missing>")
		fmt.Println("  Owner:           <unknown>")
	case preview.ConfigCorrupt:
		fmt.Println("  Name:            <unknown — config.json corrupt>")
		fmt.Println("  Owner:           <unknown>")
	default:
		fmt.Printf("  Name:            %s\n", preview.Name)
		fmt.Printf("  Owner:           %s\n", preview.Owner)
		if !preview.Created.IsZero() {
			fmt.Printf("  Created:         %s\n", preview.Created.UTC().Format("2006-01-02T15:04:05Z"))
		}
	}
	fmt.Printf("  Backing storage: %s/      (%d objects, %s)\n",
		preview.ImageID, preview.AMIObjectCount, utils.HumanBytes(utils.SafeInt64ToUint64(preview.AMIBytesTotal)))
	fmt.Printf("                   %s/ (%d objects, %s)\n",
		admin.SnapPrefix(preview.ImageID), preview.SnapObjectCount, utils.HumanBytes(utils.SafeInt64ToUint64(preview.SnapBytesTotal)))
	fmt.Println()

	// Account-owned guard before salvage / dependents — the AWS-flow hint is
	// the most useful thing to surface for this kind of mistake.
	if preview.ConfigPresent && !preview.IsSystemOwned && !force {
		fmt.Fprintf(os.Stderr,
			"Refusing to remove: %s is account-owned (%s).\n"+
				"Use 'aws ec2 deregister-image --image-id %s' followed by 'aws ec2 delete-snapshot ...'.\n",
			preview.ImageID, preview.Owner, preview.ImageID)
		os.Exit(1)
	}

	if !preview.ConfigPresent && !force {
		fmt.Fprintln(os.Stderr, "AMI config.json missing or corrupt; re-run with --force to salvage backing blocks.")
		os.Exit(1)
	}

	if !preview.Dependents.Empty() && !force {
		fmt.Fprintln(os.Stderr, "Refusing to remove: dependent resources reference this image.")
		printDependents(os.Stderr, preview.Dependents)
		fmt.Fprintln(os.Stderr, "Remove them first (e.g. 'aws ec2 terminate-instances', 'aws ec2 delete-snapshot', 'aws ec2 deregister-image') or re-run with --force.")
		os.Exit(1)
	}

	if force && (!preview.ConfigPresent || !preview.Dependents.Empty()) {
		fmt.Println("⚠️  --force: skipping dependency check and ownership check.")
		if !preview.Dependents.Empty() {
			printDependents(os.Stdout, preview.Dependents)
		}
		fmt.Println()
	}

	fmt.Println("This is permanent and cannot be undone.")
	if !yes {
		fmt.Print("Type 'yes' to proceed: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	res, err := admin.RemoveSystemImage(store, bucket, admin.RemoveImageOpts{
		ImageID: imageID,
		Force:   force,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Remove failed:", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Removed AMI %s (freed %s across %d objects).\n",
		imageID, utils.HumanBytes(utils.SafeInt64ToUint64(res.BytesFreed)), res.ObjectsDeleted)
}

func printDependents(w io.Writer, d admin.Dependents) {
	if len(d.Volumes) > 0 {
		fmt.Fprintf(w, "  Volumes (%d):\n", len(d.Volumes))
		for _, v := range d.Volumes {
			fmt.Fprintf(w, "    - %s\n", v)
		}
	}
	if len(d.Snapshots) > 0 {
		fmt.Fprintf(w, "  Snapshots (%d):\n", len(d.Snapshots))
		for _, s := range d.Snapshots {
			fmt.Fprintf(w, "    - %s\n", s)
		}
	}
	if len(d.AMIs) > 0 {
		fmt.Fprintf(w, "  AMIs (%d):\n", len(d.AMIs))
		for _, a := range d.AMIs {
			fmt.Fprintf(w, "    - %s\n", a)
		}
	}
}

func runimagesPromoteCmd(cmd *cobra.Command, args []string) {
	imageID, _ := cmd.Flags().GetString("image-id")
	yes, _ := cmd.Flags().GetBool("yes")

	cfgFile, _ := cmd.Flags().GetString("config")
	if cfgFile == "" {
		cfgFile = DefaultConfigFile()
	}

	appConfig, err := config.LoadConfig(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error loading config file:", err)
		os.Exit(1)
	}

	node := appConfig.Nodes[appConfig.Node]
	store := objectstore.NewS3ObjectStoreFromConfig(
		node.Predastore.Host,
		node.Predastore.Region,
		node.Predastore.AccessKey,
		node.Predastore.SecretKey,
	)
	bucket := node.Predastore.Bucket

	// Read current metadata for the confirmation prompt.
	meta, err := admin.GetAMIMetadata(store, bucket, imageID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to inspect AMI:", err)
		os.Exit(1)
	}

	fmt.Println("About to promote AMI to system image:")
	fmt.Println()
	fmt.Printf("  Image ID:       %s\n", imageID)
	fmt.Printf("  Name:           %s\n", meta.Name)
	fmt.Printf("  Current owner:  %s\n", meta.ImageOwnerAlias)
	fmt.Printf("  New owner:      %s\n", admin.SystemOwnerAlias)
	if !meta.CreationDate.IsZero() {
		fmt.Printf("  Created:        %s\n", meta.CreationDate.UTC().Format("2006-01-02T15:04:05Z"))
	}
	fmt.Println()
	fmt.Println("After promotion this AMI will be visible to all accounts.")

	if !yes {
		fmt.Print("Type 'yes' to proceed: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	if _, err := admin.PromoteSystemImage(store, bucket, admin.PromoteImageOpts{ImageID: imageID}); err != nil {
		fmt.Fprintln(os.Stderr, "Promote failed:", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Promoted %s to system image (owner: %s).\n", imageID, admin.SystemOwnerAlias)
}

// List remote images available.
func runimagesListCmd(cmd *cobra.Command, args []string) {
	//fmt.Println(availableImages)

	tableData := pterm.TableData{
		{"NAME", "DISTRO", "VERSION", "ARCH", "BOOT"},
	}

	// Sort A→Z then iterate.
	keys := slices.Sorted(maps.Keys(utils.AvailableImages))
	for _, k := range keys {
		img := utils.AvailableImages[k]

		//for _, img := range utils.AvailableImages {
		tableData = append(tableData, []string{img.Name, img.Distro, img.Version, img.Arch, img.BootMode})
	}

	// Create a table with the defined data.
	// The table has a header and the text in the cells is right-aligned.
	// The Render() method is used to print the table to the console.
	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Render()

	pterm.Println("To install a selected image as an AMI use:")

	pterm.Println("spx admin images import --name <image-name>")
}

// TODO: Move all logic to a module, use minimal application logic in viper commands.
func runAdminInit(cmd *cobra.Command, args []string) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "⚠️  Warning: 'spx admin init' is not running as root.")
		fmt.Fprintln(os.Stderr, "   Service user setup and CA certificate installation will be skipped.")
		fmt.Fprintln(os.Stderr, "   For production deployments, run with sudo.")
	}

	force, _ := cmd.Flags().GetBool("force")
	configDir, _ := cmd.Flags().GetString("config-dir")
	spxRoot, _ := cmd.Flags().GetString("spinifex-dir")
	region, _ := cmd.Flags().GetString("region")
	az, _ := cmd.Flags().GetString("az")
	node, _ := cmd.Flags().GetString("node")
	nodes, _ := cmd.Flags().GetInt("nodes")
	port, _ := cmd.Flags().GetInt("port")
	bindIP, _ := cmd.Flags().GetString("bind")
	advertiseFlag, _ := cmd.Flags().GetString("advertise")
	clusterBind, _ := cmd.Flags().GetString("cluster-bind")
	clusterRoutesStr, _ := cmd.Flags().GetString("cluster-routes")
	var clusterRoutes []string
	if clusterRoutesStr != "" {
		clusterRoutes = strings.Split(clusterRoutesStr, ",")
	}
	predastoreNodesStr, _ := cmd.Flags().GetString("predastore-nodes")
	formationTimeoutStr, _ := cmd.Flags().GetString("formation-timeout")
	tokenTTLStr, _ := cmd.Flags().GetString("token-ttl")
	compactionInterval, _ := cmd.Flags().GetInt("predastore-compaction-interval")
	clusterName, _ := cmd.Flags().GetString("cluster-name")
	services, _ := cmd.Flags().GetStringSlice("services")

	// Optional operator email — validated up-front so a bad address fails
	// before we touch any config state. Empty is allowed here; reset / repeat
	// inits on a box that already has an email in /etc/spinifex/spinifex.toml
	// should preserve it (see config-preservation below).
	email, _ := cmd.Flags().GetString("email")
	email = strings.TrimSpace(email)
	if email != "" {
		if err := admin.ValidateEmail(email); err != nil {
			fmt.Fprintf(os.Stderr, "--email: %v\n", err)
			os.Exit(1)
		}
	}

	// External networking flags
	externalMode, _ := cmd.Flags().GetString("external-mode")
	externalIface, _ := cmd.Flags().GetString("external-iface")
	externalSource, _ := cmd.Flags().GetString("external-source")
	externalBindBridge, _ := cmd.Flags().GetString("external-bind-bridge")
	externalPool, _ := cmd.Flags().GetString("external-pool")
	externalGateway, _ := cmd.Flags().GetString("external-gateway")
	externalPrefixLen, _ := cmd.Flags().GetInt("external-prefix-len")
	gatewayIP, _ := cmd.Flags().GetString("gateway-ip")
	gpuPassthrough, _ := cmd.Flags().GetBool("gpu-passthrough")
	ipsecEnabled, _ := cmd.Flags().GetBool("ipsec")

	// Fire telemetry in background (completes during init work, waited at end)
	noTelemetry, _ := cmd.Flags().GetBool("no-telemetry")
	if os.Getenv("SPX_NO_TELEMETRY") == "1" {
		noTelemetry = true
	}
	var telemetryWg sync.WaitGroup
	defer telemetryWg.Wait()
	if !noTelemetry {
		telemetryWg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			admin.SendTelemetry(ctx, admin.TelemetryPayload{
				MachineID:    admin.ReadMachineID(),
				Event:        "init",
				Region:       region,
				AZ:           az,
				Node:         node,
				Nodes:        nodes,
				BindIP:       bindIP,
				Version:      Version,
				ExternalMode: externalMode,
				Email:        email,
			})
		})
	}

	// Auto-detect network topology
	var poolStart, poolEnd string
	var detectedNet *admin.DetectedNetwork
	detected, err := admin.DetectNetwork()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Network auto-detection failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Use --external-mode=nat for outbound-only VMs on a non-bridgeable uplink, or specify --external-* flags manually.\n")
	} else {
		detectedNet = detected

		// Print detected topology
		fmt.Println("\n🔍 Detected network topology:")
		fmt.Printf("  %-14s %-18s %-20s %-16s %s\n", "Interface", "IP", "Subnet", "Gateway", "Role")
		for _, iface := range detected.Interfaces {
			gw := "—"
			if iface.Gateway != "" {
				gw = iface.Gateway
			}
			fmt.Printf("  %-14s %-18s %-20s %-16s %s\n", iface.Name, iface.IP, iface.Subnet, gw, strings.ToUpper(iface.Role))
		}
		if detected.LANCount == 0 {
			fmt.Println("\n  Mode: single-NIC (veth-bridged external)")
		} else {
			fmt.Printf("\n  Mode: %d LAN + 1 WAN (veth-bridged external)\n", detected.LANCount)
		}

		// Apply auto-detected values when flags not explicitly set
		if detected.WAN != nil {
			if externalIface == "" {
				externalIface = detected.WAN.Name
			}
			if externalGateway == "" {
				externalGateway = detected.WAN.Gateway
			}
			if !cmd.Flags().Changed("external-prefix-len") {
				externalPrefixLen = detected.WAN.PrefixLen
			}

			// Default mode: always "pool". Source defaults to "static"; if
			// --external-pool is omitted the validator below will error with
			// a SuggestPoolRange hint.
			if externalMode == "" && !cmd.Flags().Changed("external-mode") {
				if isNonBridgeableUplink(detected.WAN.Name) {
					fmt.Fprintf(os.Stderr, "\n❌ Detected WAN interface %s cannot be bridged (WiFi/cellular/PPP).\n", detected.WAN.Name)
					fmt.Fprintf(os.Stderr, "   Use routed NAT mode instead (outbound-only VM networking):\n")
					fmt.Fprintf(os.Stderr, "     ./scripts/setup-ovn.sh --management --nat-uplink\n")
					fmt.Fprintf(os.Stderr, "     spx admin init --external-mode=nat\n")
					os.Exit(1)
				}
				externalMode = "pool"
			}
		}
	}
	// Validate external networking flags
	if externalMode != "" && externalMode != "pool" && externalMode != "nat" {
		fmt.Fprintf(os.Stderr, "❌ Error: --external-mode must be 'pool', 'nat', or empty, got: %s\n", externalMode)
		os.Exit(1)
	}
	// A public pool alongside nat's transit pool restores EIP / public-subnet
	// parity where the operator has spare LAN IPs; without these flags nat
	// stays Tier-1-only (host-jumpbox access, no public IPs).
	natPublicPool := externalMode == "nat" && (externalPool != "" || externalSource != "")
	// natPublicGateway keeps the upstream gateway for the public pool before
	// the transit segment claims externalGateway below.
	natPublicGateway := externalGateway
	if externalMode == "nat" {
		if nodes >= 2 {
			fmt.Fprintf(os.Stderr, "❌ Error: --external-mode=nat is single-node only (v1); use --nodes=1\n")
			os.Exit(1)
		}
		if !natPublicPool && (externalBindBridge != "" || gatewayIP != "") {
			fmt.Fprintf(os.Stderr, "❌ Error: --external-bind-bridge/--gateway-ip require --external-pool or --external-source in --external-mode=nat\n")
			os.Exit(1)
		}
		if natPublicPool {
			// DHCP DORA in nat mode binds the uplink interface itself — there
			// is no br-wan (nothing is bridged in routed mode).
			src, start, end, bb, err := resolvePublicPoolFlags(externalSource, externalPool, externalBindBridge, natPublicGateway, externalIface)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
				os.Exit(1)
			}
			externalSource, poolStart, poolEnd, externalBindBridge = src, start, end, bb
		}
		// The transit segment is fixed: the host veth owns the gateway IP and
		// masquerades the /24.
		externalGateway = host.NATTransitGatewayIP
	}
	if externalMode == "pool" {
		src, start, end, bb, err := resolvePublicPoolFlags(externalSource, externalPool, externalBindBridge, externalGateway, "br-wan")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			if strings.Contains(err.Error(), "--external-pool is required") && detectedNet != nil && detectedNet.WAN != nil {
				sugStart, sugEnd := admin.SuggestPoolRange(detectedNet.WAN)
				fmt.Fprintf(os.Stderr, "   Suggested: --external-pool=%s-%s\n", sugStart, sugEnd)
			}
			os.Exit(1)
		}
		externalSource, poolStart, poolEnd, externalBindBridge = src, start, end, bb
	}
	if externalGateway != "" && net.ParseIP(externalGateway) == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: --external-gateway is not a valid IP: %s\n", externalGateway)
		os.Exit(1)
	}
	if natPublicGateway != "" && net.ParseIP(natPublicGateway) == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: --external-gateway is not a valid IP: %s\n", natPublicGateway)
		os.Exit(1)
	}
	if gatewayIP != "" && net.ParseIP(gatewayIP) == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: --gateway-ip is not a valid IP: %s\n", gatewayIP)
		os.Exit(1)
	}

	// Detect DNS servers from the host for VM DHCP
	var dnsServers []string
	if externalMode != "" {
		dnsServers = detectDNSServers(externalIface)
		if len(dnsServers) > 0 {
			fmt.Printf("  DNS servers: %s\n", strings.Join(dnsServers, ", "))
		}
	}

	// Assemble the external pool blocks rendered into spinifex.toml.
	var externalPools []admin.PoolData
	switch externalMode {
	case "nat":
		externalPools = append(externalPools, admin.PoolData{
			Name: host.NATTransitPoolName, Gateway: host.NATTransitGatewayIP,
			PrefixLen: 24, DNSServers: dnsServers,
			GwLrpRangeStart: host.NATTransitGwLrpStart, GwLrpRangeEnd: host.NATTransitGwLrpEnd,
		})
		if natPublicPool {
			// WiFi/WWAN uplinks drop frames with foreign source MACs, so DHCP
			// leases must go out with the interface's own MAC.
			dhcpMAC := ""
			if externalSource == "dhcp" && isNonBridgeableUplink(externalBindBridge) {
				dhcpMAC = "interface"
			}
			externalPools = append(externalPools, admin.PoolData{
				Name: "wan", Source: externalSource, BindBridge: externalBindBridge,
				DHCPMAC: dhcpMAC, Start: poolStart, End: poolEnd, Gateway: natPublicGateway,
				GatewayIP: gatewayIP, PrefixLen: externalPrefixLen, DNSServers: dnsServers,
			})
		}
	case "pool":
		externalPools = append(externalPools, admin.PoolData{
			Name: "wan", Source: externalSource, BindBridge: externalBindBridge,
			Start: poolStart, End: poolEnd, Gateway: externalGateway,
			GatewayIP: gatewayIP, PrefixLen: externalPrefixLen, DNSServers: dnsServers,
		})
	}

	// Validate IP address format
	if net.ParseIP(bindIP) == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: Invalid IP address for --bind: %s\n", bindIP)
		os.Exit(1)
	}

	// Resolve the off-host advertise IP. DetectNetwork may have failed earlier;
	// detect lazily if we still need the WAN IP.
	if advertiseFlag == "" && (bindIP == "0.0.0.0" || bindIP == "127.0.0.1") && detectedNet == nil {
		if d, derr := admin.DetectNetwork(); derr == nil {
			detectedNet = d
		}
	}
	advertiseIP, err := resolveAdvertiseIP(bindIP, advertiseFlag, detectedNet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}

	// Validate port range
	if port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "❌ Error: Port must be between 1 and 65535, got: %d\n", port)
		os.Exit(1)
	}

	// Default cluster-bind to bind IP if not specified
	if clusterBind == "" {
		clusterBind = bindIP
	}

	fmt.Printf("Initializing Spinifex with bind IP: %s, advertise IP: %s, port: %d\n", bindIP, advertiseIP, port)

	// Default config directory
	if configDir == "" {
		configDir = DefaultConfigDir()
	}

	fmt.Println("🚀 Initializing Spinifex platform...")
	fmt.Printf("Configuration directory: %s\n\n", configDir)

	// Check if already initialized
	spinifexTomlPath := filepath.Join(configDir, "spinifex.toml")
	if !force && admin.FileExists(spinifexTomlPath) {
		fmt.Println("⚠️  Spinifex already initialized!")
		fmt.Printf("Config file exists: %s\n", spinifexTomlPath)
		fmt.Println("\nTo re-initialize, run with --force flag:")
		fmt.Println("  spx admin init --force")
		os.Exit(0)
	}

	// Preserve the previously-captured operator email across --force re-inits
	// when --email is omitted (e.g. reset-dev-env.sh workflows). Without this
	// a reset would silently blank the address.
	if email == "" && admin.FileExists(spinifexTomlPath) {
		email = admin.ReadOperatorEmail(spinifexTomlPath)
	}

	// Create config directory
	if err := os.MkdirAll(configDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config directory: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Created config directory: %s\n", configDir)

	// Identity and crypto material is load-or-generate: a fresh install mints a
	// new identity bundle, but a --force re-init preserves the existing one so
	// data sealed under it (NATS KV secrets, sealed fragments, encrypted volumes)
	// stays decryptable.
	masterKey, masterKeyExisted, err := ensureMasterKey(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing IAM master key: %v\n", err)
		os.Exit(1)
	}
	accountID := admin.SystemAccountID()
	bootstrapDir := filepath.Join(spxRoot, "awsgw")

	var accessKey, secretKey, adminAccessKey, adminSecretKey string
	if masterKeyExisted {
		// Preserve path: reuse the existing identity. The system credentials must
		// match what seeded the NATS KV `system` secret, so load them rather than
		// mint new ones. The admin credentials are not recovered: bootstrap.json is
		// consumed and deleted by awsgw after first boot, and the operator's copy
		// already lives in ~/.aws/credentials. Leaving them empty makes
		// finalizeNodeSetup refresh only ~/.aws/config (endpoint/CA for a changed
		// bind IP), not the credentials.
		accessKey, secretKey, err = loadSystemCredentials(configDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading preserved system credentials: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("\n🔐 Preserved existing identity (master key, credentials and CA unchanged)")
		fmt.Printf("   Master key: %s\n", filepath.Join(configDir, "master.key"))
	} else {
		// Fresh install: mint system + admin credentials and seed the bootstrap files.
		accessKey, err = admin.GenerateAWSAccessKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating access key: %v\n", err)
			os.Exit(1)
		}
		secretKey, err = admin.GenerateAWSSecretKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating secret key: %v\n", err)
			os.Exit(1)
		}
		bootstrapResult, err := writeBootstrapFiles(configDir, bootstrapDir, masterKey, accessKey, secretKey, accountID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing bootstrap files: %v\n", err)
			os.Exit(1)
		}
		if err := writeSystemCredentials(configDir, accessKey, secretKey); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing system credentials: %v\n", err)
			os.Exit(1)
		}
		adminAccessKey = bootstrapResult.AdminAccessKey
		adminSecretKey = bootstrapResult.AdminSecretKey
		fmt.Println("\n🔐 Generated IAM master key")
		fmt.Printf("   Master key: %s\n", filepath.Join(configDir, "master.key"))
		fmt.Printf("   Bootstrap: %s\n", filepath.Join(bootstrapDir, "bootstrap.json"))
		fmt.Printf("   System creds: %s\n", filepath.Join(configDir, "system-credentials.json"))
	}

	// Predastore encryption key is per-node and never transmitted; load-or-generate
	// so the service has it on first start and a re-init keeps sealed fragments.
	predastoreKeyPath, err := writePredastoreEncryptionKey(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing predastore encryption key: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n🔐 Predastore encryption key ready (per-node, never transmitted)")
	fmt.Printf("   Key: %s\n", predastoreKeyPath)

	// Viperblock at-rest encryption key is cluster-wide; load-or-generate so a
	// re-init keeps the existing key (and its sealed volumes). The bytes feed the
	// multi-node leader's key distribution below.
	viperblockKey, viperblockKeyPath, err := ensureViperblockEncryptionKey(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing viperblock encryption key: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n🔐 Viperblock at-rest encryption key ready")
	fmt.Printf("   Key: %s\n", viperblockKeyPath)

	if !masterKeyExisted {
		fmt.Printf("\n🔑 Generated admin credentials (save these — they won't be shown again):\n")
		fmt.Printf("   Access Key:  %s\n", adminAccessKey)
		fmt.Printf("   Secret Key:  %s\n", adminSecretKey)
		fmt.Printf("   Account:     %s (%s)\n", admin.DefaultAccountName(), admin.DefaultAccountID())
		fmt.Printf("   AWS Profile: spinifex\n")
	}

	// Generate SSL certificates (with bind IP in SANs for multi-node support)
	certPath := admin.GenerateCertificatesIfNeeded(configDir, force, bindIP, region, config.DefaultAWSInternalSuffix)

	// Generate per-node IPsec peer cert when cluster-wide IPsec is enabled
	// (default true). Reuses the cluster CA — no intermediate strongSwan PKI.
	if ipsecEnabled {
		caCertPath := filepath.Join(configDir, "ca.pem")
		caKeyPath := filepath.Join(configDir, "ca.key")
		if err := admin.GenerateIPSecPeerCert(configDir, caCertPath, caKeyPath, node, bindIP); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating IPsec peer certificate: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("🔐 IPsec peer certificate generated (intra-AZ Geneve encryption ON)")
	}

	// Install CA certificate into system trust store
	installCACertificate(filepath.Join(configDir, "ca.pem"))

	// Generate NATS token
	natsToken, err := admin.GenerateNATSToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating NATS token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n🔒 Generated NATS authentication token")

	if spxRoot == "" {
		spxRoot = DefaultDataDir()
	}
	spxRoot = filepath.Clean(spxRoot)

	// Generate dedicated, bucket-scoped credentials for the northstar DNS
	// service. Rendered into predastore.toml ([[auth]]) and northstar.toml so
	// the resolver reads zone files read-only from its own S3 bucket.
	//
	// Generated above the multi-node dispatch because the pair is cluster-wide:
	// a node's predastore only honours the keys rendered into its own config, so
	// every node must present this same pair to read the distributed zone bucket.
	northstarAccessKey, err := admin.GenerateAWSAccessKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating northstar access key: %v\n", err)
		os.Exit(1)
	}
	northstarSecretKey, err := admin.GenerateAWSSecretKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating northstar secret key: %v\n", err)
		os.Exit(1)
	}
	northstarCreds := admin.NorthstarCredentials{
		AccessKey: northstarAccessKey,
		SecretKey: northstarSecretKey,
		Bucket:    admin.NorthstarBucketName,
	}

	// Determine if this is a multi-node formation. Operator intent comes from
	// --nodes, not from whether --bind was left at the 0.0.0.0 default.
	isMultiNode := nodes >= 2

	if isMultiNode {
		// Build cluster-wide network config for propagation to joining nodes.
		// Always emit so the IPSecEnabled flag reaches joiners even when
		// external networking is disabled.
		networkConfig := &formation.NetworkConfig{
			IPSecEnabled: ipsecEnabled,
		}
		if externalMode != "" {
			bootstrapVpcId := utils.GenerateResourceID("vpc")
			bootstrapSubnetId := utils.GenerateResourceID("subnet")
			bootstrapIgwId := utils.GenerateResourceID("igw")
			networkConfig.ExternalMode = externalMode
			networkConfig.PoolName = "wan"
			networkConfig.PoolSource = externalSource
			networkConfig.PoolBindBridge = externalBindBridge
			networkConfig.PoolStart = poolStart
			networkConfig.PoolEnd = poolEnd
			networkConfig.PoolGateway = externalGateway
			networkConfig.PoolGatewayIP = gatewayIP
			networkConfig.PoolPrefixLen = externalPrefixLen
			networkConfig.PoolDNSServers = dnsServers
			networkConfig.BootstrapAccountId = admin.DefaultAccountID()
			networkConfig.BootstrapVpcId = bootstrapVpcId
			networkConfig.BootstrapSubnetId = bootstrapSubnetId
			networkConfig.BootstrapIgwId = bootstrapIgwId
			networkConfig.BootstrapCidr = handlers_ec2_vpc.DefaultVPCCidr
			networkConfig.BootstrapSubnetCidr = handlers_ec2_vpc.DefaultSubnetCidr
		}

		runAdminInitMultiNode(cmd, accessKey, secretKey, accountID, adminAccessKey, adminSecretKey,
			masterKey, viperblockKey, natsToken, clusterName,
			configDir, spxRoot, certPath, region, az, node, bindIP, advertiseIP, clusterBind, email,
			port, nodes, formationTimeoutStr, tokenTTLStr, services, networkConfig, northstarCreds)
		return
	}

	// --- Single-node path (existing behavior) ---

	// Create config files from embedded templates
	fmt.Println("\n📝 Creating configuration files...")

	dirs, err := createConfigSubdirs(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config subdirectories: %v\n", err)
		os.Exit(1)
	}

	portStr := strconv.Itoa(port)

	// The keys are cluster-wide and generated above the dispatch; the config path
	// is node-local, so it is derived here from this node's own config dirs.
	northstarConfigPath := filepath.Join(dirs.Northstar, "northstar.toml")

	// Parse multi-node predastore configuration (legacy flag-based approach for single-node)
	var predastoreNodeID int
	if predastoreNodesStr != "" {
		ips := strings.Split(predastoreNodesStr, ",")
		if len(ips) < 2 {
			fmt.Fprintf(os.Stderr, "❌ Error: --predastore-nodes requires at least 2 IPs, got %d\n", len(ips))
			os.Exit(1)
		}

		var predastoreNodes []admin.PredastoreNodeConfig
		for i, ip := range ips {
			ip = strings.TrimSpace(ip)
			if net.ParseIP(ip) == nil {
				fmt.Fprintf(os.Stderr, "❌ Error: Invalid IP in --predastore-nodes: %s\n", ip)
				os.Exit(1)
			}
			predastoreNodes = append(predastoreNodes, admin.PredastoreNodeConfig{
				ID:   i + 1,
				Host: ip,
			})
		}

		predastoreNodeID = admin.FindNodeIDByIP(predastoreNodes, bindIP)
		if predastoreNodeID == 0 {
			fmt.Fprintf(os.Stderr, "❌ Error: --bind IP %s not found in --predastore-nodes list\n", bindIP)
			os.Exit(1)
		}

		// Generate multi-node predastore.toml
		predastoreContent, err := admin.GenerateMultiNodePredastoreConfig(predastoreMultiNodeTemplate, predastoreNodes, accessKey, secretKey, region, natsToken, configDir, bindIP, compactionInterval, northstarCreds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating multi-node predastore config: %v\n", err)
			os.Exit(1)
		}

		predastorePath := filepath.Join(dirs.Predastore, "predastore.toml")
		if err := os.WriteFile(predastorePath, []byte(predastoreContent), 0600); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing predastore config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Created: multi-node predastore.toml (node ID: %d)\n", predastoreNodeID)
	}

	// Pre-generate default VPC/subnet/IGW IDs for bootstrap config.
	// These are written to [bootstrap] in spinifex.toml so vpcd can
	// create OVN topology on first boot. The daemon uses the same IDs
	// when it creates the records in NATS KV via EnsureDefaultVPC.
	bootstrapVpcId := utils.GenerateResourceID("vpc")
	bootstrapSubnetId := utils.GenerateResourceID("subnet")
	bootstrapIgwId := utils.GenerateResourceID("igw")

	configSettings := admin.ConfigSettings{
		AccessKey: accessKey,
		SecretKey: secretKey,
		AccountID: accountID,
		Region:    region,
		NatsToken: natsToken,
		DataDir:   spxRoot,
		LogDir:    LogDirFor(spxRoot),
		ConfigDir: configDir,

		Node:          node,
		Az:            az,
		Port:          portStr,
		BindIP:        bindIP,
		AdvertiseIP:   advertiseIP,
		ClusterBindIP: clusterBind,
		ClusterRoutes: clusterRoutes,
		ClusterName:   clusterName,

		PredastoreNodeID:          predastoreNodeID,
		CompactionIntervalSeconds: compactionInterval,
		Services:                  services,

		OVNNBAddr: "tcp:127.0.0.1:6641",
		OVNSBAddr: "tcp:127.0.0.1:6642",

		ExternalMode:  externalMode,
		ExternalIface: externalIface,
		BridgeMode:    bridgeModeFor(externalMode),
		Pools:         externalPools,

		OperatorEmail:       email,
		BootstrapAccountId:  admin.DefaultAccountID(),
		BootstrapVpcId:      bootstrapVpcId,
		BootstrapSubnetId:   bootstrapSubnetId,
		BootstrapIgwId:      bootstrapIgwId,
		BootstrapCidr:       handlers_ec2_vpc.DefaultVPCCidr,
		BootstrapSubnetCidr: handlers_ec2_vpc.DefaultSubnetCidr,

		GPUPassthrough: gpuPassthrough,
		IPSecEnabled:   ipsecEnabled,

		EncryptionKeyFile: viperblockKeyPath,

		NorthstarAccessKey:      northstarCreds.AccessKey,
		NorthstarSecretKey:      northstarCreds.SecretKey,
		NorthstarBucket:         northstarCreds.Bucket,
		NorthstarDefaultDomain:  admin.NorthstarDefaultDomain,
		NorthstarInternalDomain: admin.NorthstarInternalDomain,
		NorthstarConfigPath:     northstarConfigPath,
		PoolDNSServers:          dnsServers,
	}

	// Print external networking summary
	if externalMode == "nat" {
		if natPublicPool {
			fmt.Printf("\n📡 External networking: nat (routed) with public pool — EIPs enabled\n")
			if externalSource == "static" {
				fmt.Printf("  Public pool:   %s - %s (source: static)\n", poolStart, poolEnd)
			} else {
				fmt.Printf("  Public pool:   dhcp via %s\n", externalBindBridge)
			}
		} else {
			fmt.Printf("\n📡 External networking: nat (routed, outbound-only — no public IPs/EIPs)\n")
		}
		fmt.Printf("  Transit:       %s via %s (host masquerades out any uplink)\n", host.NATTransitCIDR, host.NATTransitHostEnd)
		fmt.Printf("  Host setup:    ./scripts/setup-ovn.sh --nat-uplink (run before starting services)\n")
	} else if externalMode != "" {
		fmt.Printf("\n📡 External networking: %s\n", externalMode)
		fmt.Printf("  WAN interface: %s\n", externalIface)
		switch externalSource {
		case "static":
			fmt.Printf("  Source:        static (IP range)\n")
			fmt.Printf("  IP pool:       %s - %s\n", poolStart, poolEnd)
			fmt.Printf("  ⚠️  Ensure %s-%s is excluded from your router's DHCP range.\n", poolStart, poolEnd)
		case "dhcp":
			fmt.Printf("  Source:        dhcp (upstream DHCP server)\n")
			fmt.Printf("  Bind bridge:   %s\n", externalBindBridge)
		}
		if gatewayIP != "" {
			fmt.Printf("  Gateway IP:    %s (static)\n", gatewayIP)
		}
	}

	// Generate config files
	if err := generateAndWriteConfigs(dirs, spinifexTomlPath, configSettings, predastoreNodesStr != ""); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating configuration files: %v\n", err)
		os.Exit(1)
	}

	finalizeNodeSetup(spxRoot, certPath, adminAccessKey, adminSecretKey, region, bindIP)

	// Write node.conf so spx admin banner works on source installs (not just ISO).
	nodeHostname, _ := os.Hostname()
	if nodeHostname == "" {
		nodeHostname = node
	}
	nodeConfPath := filepath.Join(configDir, "node.conf")
	if err := writeNodeConf(nodeConfPath, map[string]string{
		"MANAGEMENT_IP":    advertiseIP,
		"MANAGEMENT_IFACE": "br-wan",
		"NODE_HOSTNAME":    nodeHostname,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: could not write %s: %v\n", nodeConfPath, err)
	}

	// Print success message
	fmt.Println("\n🎉 Spinifex initialization complete!")
	fmt.Println()
	fmt.Println("🔗 Configuration:")
	fmt.Printf("   Config file: %s\n", spinifexTomlPath)
	fmt.Printf("   Data directory: %s\n", spxRoot)
	fmt.Printf("   Bind IP: %s (listen)\n", bindIP)
	fmt.Printf("   Advertise IP: %s (off-host dial target)\n", advertiseIP)
	fmt.Printf("   Loopback: 127.0.0.1 (in-process dial target)\n")
	fmt.Println()
}

// runAdminInitMultiNode handles the multi-node formation path for admin init.
// It starts a formation server, registers this node, waits for all nodes to join,
// then generates configs with complete cluster topology.
//
// The northstar credentials are generated by the caller and provision this
// node's predastore with the zone bucket. The same pair is distributed to
// joiners, since each node's predastore only honours the keys in its own config.
func runAdminInitMultiNode(cmd *cobra.Command, accessKey, secretKey, accountID, adminAccessKey, adminSecretKey string,
	masterKey, viperblockKey []byte, natsToken, clusterName,
	configDir, spxRoot, certPath, region, az, node, bindIP, advertiseIP, clusterBind, email string,
	port, expectedNodes int, formationTimeoutStr, tokenTTLStr string, services []string, networkConfig *formation.NetworkConfig,
	northstarCreds admin.NorthstarCredentials) {
	formationTimeout, err := time.ParseDuration(formationTimeoutStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: Invalid --formation-timeout: %v\n", err)
		os.Exit(1)
	}

	tokenTTL, err := time.ParseDuration(tokenTTLStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: Invalid --token-ttl: %v\n", err)
		os.Exit(1)
	}
	if tokenTTL < formationTimeout+1*time.Minute {
		fmt.Fprintf(os.Stderr, "❌ Error: --token-ttl (%s) must be >= --formation-timeout + 1m (%s)\n", tokenTTL, formationTimeout+1*time.Minute)
		os.Exit(1)
	}

	compactionInterval, _ := cmd.Flags().GetInt("predastore-compaction-interval")

	joinToken, err := formation.GenerateJoinToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error generating join token: %v\n", err)
		os.Exit(1)
	}

	// Identity, predastore and viperblock keys were prepared load-or-generate by
	// the caller: reuse them here so a re-init never rotates cluster crypto and
	// the leader distributes the same viperblock key to joiners. The path is
	// deterministic and already written on disk.
	viperblockKeyPath := filepath.Join(configDir, "viperblock", "encryption.key")

	// Read CA cert/key for distribution to joining nodes
	caCertData, err := os.ReadFile(filepath.Join(configDir, "ca.pem"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading CA cert: %v\n", err)
		os.Exit(1)
	}
	caKeyData, err := os.ReadFile(filepath.Join(configDir, "ca.key"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading CA key: %v\n", err)
		os.Exit(1)
	}

	creds := &formation.SharedCredentials{
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		AccountID:      accountID,
		NatsToken:      natsToken,
		ClusterName:    clusterName,
		Region:         region,
		AdminAccessKey: adminAccessKey,
		AdminSecretKey: adminSecretKey,

		// Joiners provision their own predastore with this pair, so every
		// node's resolver can read the distributed zone bucket via its local
		// endpoint. The bucket is a constant, so it is derived node-side.
		NorthstarAccessKey: northstarCreds.AccessKey,
		NorthstarSecretKey: northstarCreds.SecretKey,
	}

	fs := formation.NewFormationServer(expectedNodes, creds, string(caCertData), string(caKeyData), networkConfig, joinToken, tokenTTL)

	// Include master key in formation server for distribution to joining nodes
	fs.SetMasterKey(base64.StdEncoding.EncodeToString(masterKey))

	// Distribute the cluster-wide viperblock encryption key to joiners
	fs.SetViperblockKey(base64.StdEncoding.EncodeToString(viperblockKey))

	// Register self (init node) as the first node
	selfNode := formation.NodeInfo{
		Name:        node,
		BindIP:      bindIP,
		AdvertiseIP: advertiseIP,
		ClusterIP:   clusterBind,
		Region:      region,
		AZ:          az,
		Port:        port,
	}
	if err := fs.RegisterNode(selfNode); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error registering self: %v\n", err)
		os.Exit(1)
	}

	// Write join token to file for automated workflows
	tokenPath := filepath.Join(configDir, "join-token")
	if err := os.WriteFile(tokenPath, []byte(joinToken), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error writing join token: %v\n", err)
		os.Exit(1)
	}

	// Start formation server
	formationAddr := fmt.Sprintf("%s:%d", bindIP, port)
	if err := fs.Start(formationAddr); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error starting formation server: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n📡 Formation server started on %s\n", formationAddr)
	fmt.Printf("   Waiting for %d more node(s) to join...\n", expectedNodes-1)
	fmt.Printf("   Token expires in %s\n\n", tokenTTL)
	fmt.Printf("   Other nodes should run:\n")
	fmt.Printf("   sudo spx admin join --host %s --token %s --node <name> --bind <ip>\n\n", formationAddr, joinToken)

	// Wait for all nodes to register
	if err := fs.WaitForCompletion(formationTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		fs.Shutdown(context.Background())
		os.Remove(tokenPath)
		os.Exit(1)
	}

	fmt.Printf("✅ All %d nodes joined!\n", expectedNodes)

	// Build cluster topology from formation data
	allNodes := fs.Nodes()
	clusterRoutes := formation.BuildClusterRoutes(allNodes)
	predastoreNodes := formation.BuildPredastoreNodes(allNodes)
	ovnNBAddr, ovnSBAddr := formation.BuildOVNDBAddrs(allNodes)

	fmt.Println("\n📝 Creating configuration files...")

	dirs, err := createConfigSubdirs(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config subdirectories: %v\n", err)
		os.Exit(1)
	}

	portStr := strconv.Itoa(port)

	// The keys are cluster-wide and generated above the dispatch; the config path
	// is node-local, so it is derived here from this node's own config dirs.
	northstarConfigPath := filepath.Join(dirs.Northstar, "northstar.toml")

	// Generate multi-node predastore config
	var predastoreNodeID int
	hasPredastoreConfig := len(predastoreNodes) >= 2
	if hasPredastoreConfig {
		predastoreContent, err := admin.GenerateMultiNodePredastoreConfig(predastoreMultiNodeTemplate, predastoreNodes, accessKey, secretKey, region, natsToken, configDir, bindIP, compactionInterval, northstarCreds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating multi-node predastore config: %v\n", err)
			os.Exit(1)
		}

		predastorePath := filepath.Join(dirs.Predastore, "predastore.toml")
		if err := os.WriteFile(predastorePath, []byte(predastoreContent), 0600); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing predastore config: %v\n", err)
			os.Exit(1)
		}

		predastoreNodeID = admin.FindNodeIDByIP(predastoreNodes, bindIP)
		fmt.Printf("✅ Created: multi-node predastore.toml (node ID: %d)\n", predastoreNodeID)
	}

	spinifexTomlPath := filepath.Join(configDir, "spinifex.toml")

	configSettings := admin.ConfigSettings{
		AccessKey: accessKey,
		SecretKey: secretKey,
		AccountID: accountID,
		Region:    region,
		NatsToken: natsToken,
		DataDir:   spxRoot,
		LogDir:    LogDirFor(spxRoot),
		ConfigDir: configDir,

		Node:          node,
		Az:            az,
		Port:          portStr,
		BindIP:        bindIP,
		AdvertiseIP:   advertiseIP,
		ClusterBindIP: clusterBind,
		ClusterRoutes: clusterRoutes,
		ClusterName:   clusterName,

		PredastoreNodeID:          predastoreNodeID,
		CompactionIntervalSeconds: compactionInterval,
		Services:                  services,
		RemoteNodes:               buildRemoteNodes(allNodes, node, northstarConfigPath),

		OperatorEmail: email,

		EncryptionKeyFile: viperblockKeyPath,

		NorthstarAccessKey:      northstarCreds.AccessKey,
		NorthstarSecretKey:      northstarCreds.SecretKey,
		NorthstarBucket:         northstarCreds.Bucket,
		NorthstarDefaultDomain:  admin.NorthstarDefaultDomain,
		NorthstarInternalDomain: admin.NorthstarInternalDomain,
		NorthstarConfigPath:     northstarConfigPath,

		// Multi-endpoint OVN NB/SB list across the RAFT quorum; the init node's
		// own address leads, the rest provide failover.
		OVNNBAddr: ovnNBAddr,
		OVNSBAddr: ovnSBAddr,
	}

	if networkConfig != nil {
		applyNetworkConfig(&configSettings, networkConfig)
	}

	if err := generateAndWriteConfigs(dirs, spinifexTomlPath, configSettings, hasPredastoreConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating configuration files: %v\n", err)
		os.Exit(1)
	}

	finalizeNodeSetup(spxRoot, certPath, adminAccessKey, adminSecretKey, region, bindIP)

	// Keep formation server running briefly so joining nodes can fetch complete status
	fmt.Println("\n⏳ Waiting for joining nodes to fetch cluster data...")
	time.Sleep(15 * time.Second)

	// Shutdown formation server
	fs.Shutdown(context.Background())
	if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("Failed to remove join token file", "path", tokenPath, "error", err)
	}

	// Print cluster summary
	fmt.Println("\n🎉 Cluster formation complete!")
	fmt.Printf("   Cluster: %s (%d nodes)\n", clusterName, expectedNodes)
	fmt.Printf("   Region: %s\n", region)
	fmt.Printf("   Bind: %s  Advertise: %s  Loopback: 127.0.0.1\n", bindIP, advertiseIP)
	fmt.Println("   Nodes:")
	for name, n := range allNodes {
		adv := n.AdvertiseIP
		if adv == "" {
			adv = n.BindIP
		}
		fmt.Printf("     - %s (bind=%s advertise=%s)\n", name, n.BindIP, adv)
	}
	fmt.Println("\n📋 Next steps:")
	fmt.Println("   1. Start services on ALL nodes:")
	fmt.Println("      sudo systemctl start spinifex.target")
	fmt.Println()
}

func runAdminJoin(cmd *cobra.Command, args []string) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "⚠️  Warning: 'spx admin join' is not running as root.")
		fmt.Fprintln(os.Stderr, "   Service user setup and CA certificate installation will be skipped.")
		fmt.Fprintln(os.Stderr, "   For production deployments, run with sudo.")
	}

	node, _ := cmd.Flags().GetString("node")
	leaderHost, _ := cmd.Flags().GetString("host")
	joinToken, _ := cmd.Flags().GetString("token")
	region, _ := cmd.Flags().GetString("region")
	az, _ := cmd.Flags().GetString("az")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	port, _ := cmd.Flags().GetInt("port")
	bindIP, _ := cmd.Flags().GetString("bind")
	advertiseFlag, _ := cmd.Flags().GetString("advertise")
	configDir, _ := cmd.Flags().GetString("config-dir")
	clusterBind, _ := cmd.Flags().GetString("cluster-bind")
	services, _ := cmd.Flags().GetStringSlice("services")
	compactionInterval, _ := cmd.Flags().GetInt("predastore-compaction-interval")

	email, _ := cmd.Flags().GetString("email")
	email = strings.TrimSpace(email)
	if email != "" {
		if err := admin.ValidateEmail(email); err != nil {
			fmt.Fprintf(os.Stderr, "--email: %v\n", err)
			os.Exit(1)
		}
	} else if admin.FileExists(filepath.Join(configDir, "spinifex.toml")) {
		email = admin.ReadOperatorEmail(filepath.Join(configDir, "spinifex.toml"))
	}

	// Validate required parameters
	if node == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: --node is required\n")
		os.Exit(1)
	}
	if leaderHost == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: --host is required\n")
		os.Exit(1)
	}

	// Validate IP address format
	if net.ParseIP(bindIP) == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: Invalid IP address for --bind: %s\n", bindIP)
		os.Exit(1)
	}

	// Resolve the off-host advertise IP before the JoinRequest is built so
	// peers record a reachable dial target instead of 0.0.0.0.
	var detectedNet *admin.DetectedNetwork
	if advertiseFlag == "" && (bindIP == "0.0.0.0" || bindIP == "127.0.0.1") {
		if d, derr := admin.DetectNetwork(); derr == nil {
			detectedNet = d
		}
	}
	advertiseIP, err := resolveAdvertiseIP(bindIP, advertiseFlag, detectedNet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}

	// Validate port range
	if port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "❌ Error: Port must be between 1 and 65535, got: %d\n", port)
		os.Exit(1)
	}

	// Default cluster-bind to bind IP if not specified
	if clusterBind == "" {
		clusterBind = bindIP
	}

	// Set default data directory
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}

	fmt.Println("🚀 Joining Spinifex cluster...")
	fmt.Printf("Node: %s\n", node)
	fmt.Printf("Leader: %s\n", leaderHost)
	fmt.Printf("Region: %s\n", region)
	fmt.Printf("AZ: %s\n", az)
	fmt.Printf("Bind IP: %s\n", bindIP)
	fmt.Printf("Advertise IP: %s\n", advertiseIP)
	fmt.Printf("Port: %d\n\n", port)

	// POST join request to formation server
	joinReq := formation.JoinRequest{
		NodeInfo: formation.NodeInfo{
			Name:        node,
			BindIP:      bindIP,
			AdvertiseIP: advertiseIP,
			ClusterIP:   clusterBind,
			Region:      region,
			AZ:          az,
			Port:        port,
		},
	}

	reqBody, err := json.Marshal(joinReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling join request: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // formation server uses ephemeral self-signed cert
		},
	}

	joinURL := fmt.Sprintf("https://%s/formation/join", leaderHost)
	req, err := http.NewRequest(http.MethodPost, joinURL, bytes.NewBuffer(reqBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error creating join request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+joinToken)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error connecting to formation server: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the leader node has run 'spx admin init' and is accessible at %s\n", leaderHost)
		os.Exit(1)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading response body: %v\n", err)
		os.Exit(1)
	}

	var joinResp formation.JoinResponse
	if err := json.Unmarshal(body, &joinResp); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing join response: %v\n", err)
		os.Exit(1)
	}

	if !joinResp.Success {
		fmt.Fprintf(os.Stderr, "❌ Failed to join cluster: %s\n", joinResp.Message)
		os.Exit(1)
	}

	fmt.Printf("✅ Registered with formation server (%d/%d nodes joined)\n", joinResp.Joined, joinResp.Expected)

	// Poll status until formation is complete
	statusURL := fmt.Sprintf("https://%s/formation/status", leaderHost)
	var statusResp formation.StatusResponse

	for {
		statusReq, err := http.NewRequest(http.MethodGet, statusURL, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error creating status request: %v\n", err)
			os.Exit(1)
		}
		statusReq.Header.Set("Authorization", "Bearer "+joinToken)

		sResp, err := client.Do(statusReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error polling formation status: %v\n", err)
			os.Exit(1)
		}

		sBody, err := io.ReadAll(sResp.Body)
		sResp.Body.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error reading status response: %v\n", err)
			os.Exit(1)
		}

		if sResp.StatusCode == http.StatusUnauthorized {
			fmt.Fprintf(os.Stderr, "❌ Error: join token rejected by formation server (expired or invalid)\n")
			os.Exit(1)
		}
		if sResp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "❌ Error: unexpected status %d from formation server\n", sResp.StatusCode)
			os.Exit(1)
		}

		if err := json.Unmarshal(sBody, &statusResp); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error parsing status response: %v\n", err)
			os.Exit(1)
		}

		if statusResp.Complete {
			break
		}

		fmt.Printf("   Waiting for cluster formation... (%d/%d nodes joined)\n", statusResp.Joined, statusResp.Expected)
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("✅ Cluster formation complete! (%d nodes)\n\n", statusResp.Expected)

	// Fire telemetry after formation (now we know the cluster topology)
	noTelemetry, _ := cmd.Flags().GetBool("no-telemetry")
	if os.Getenv("SPX_NO_TELEMETRY") == "1" {
		noTelemetry = true
	}
	var telemetryWg sync.WaitGroup
	defer telemetryWg.Wait()
	if !noTelemetry {
		telemetryWg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			admin.SendTelemetry(ctx, admin.TelemetryPayload{
				MachineID: admin.ReadMachineID(),
				Event:     "join",
				Region:    region,
				AZ:        az,
				Node:      node,
				Nodes:     statusResp.Expected,
				BindIP:    bindIP,
				Version:   Version,
				Email:     email,
			})
		})
	}

	// Extract credentials and CA from formation status
	creds := statusResp.Credentials
	if creds == nil {
		fmt.Fprintf(os.Stderr, "❌ Error: formation server did not return credentials\n")
		os.Exit(1)
	}

	// Set up config directory
	if configDir == "" {
		configDir = DefaultConfigDir()
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config directory: %v\n", err)
		os.Exit(1)
	}

	// Write CA cert and key
	caCertPath := filepath.Join(configDir, "ca.pem")
	caKeyPath := filepath.Join(configDir, "ca.key")

	if statusResp.CACert == "" || statusResp.CAKey == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: formation server did not return CA certificate\n")
		os.Exit(1)
	}

	if err := os.WriteFile(caCertPath, []byte(statusResp.CACert), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing CA cert: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(caKeyPath, []byte(statusResp.CAKey), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing CA key: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ CA certificate received from leader: %s\n", caCertPath)

	// Install CA certificate into system trust store
	installCACertificate(caCertPath)

	// Extract and write master key from formation server
	if statusResp.MasterKey == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: formation server did not return master key\n")
		os.Exit(1)
	}
	masterKeyBytes, err := base64.StdEncoding.DecodeString(statusResp.MasterKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error decoding master key: %v\n", err)
		os.Exit(1)
	}
	bootstrapDir := filepath.Join(dataDir, "awsgw")
	if err := writeBootstrapFilesWithAdmin(configDir, bootstrapDir, masterKeyBytes, creds.AccessKey, creds.SecretKey, creds.AccountID, creds.AdminAccessKey, creds.AdminSecretKey); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error writing bootstrap files: %v\n", err)
		os.Exit(1)
	}
	if err := writeSystemCredentials(configDir, creds.AccessKey, creds.SecretKey); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error writing system credentials: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ IAM master key received from leader")
	fmt.Printf("✅ Bootstrap file written: %s\n", filepath.Join(bootstrapDir, "bootstrap.json"))

	// Predastore encryption key is per-node: generate locally rather than
	// receiving from the leader. Each node only opens fragments it sealed
	// itself, so there is no cluster-wide predastore key to share.
	predastoreKeyPath, err := writePredastoreEncryptionKey(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error generating predastore encryption key: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Predastore encryption key generated: %s\n", predastoreKeyPath)

	// Viperblock at-rest encryption key is cluster-wide: receive it from the
	// leader rather than generating one, so this node can open volumes sealed
	// elsewhere. Lenient on absence — an older leader that predates key
	// distribution returns nothing; fall back to cleartext rather than fail
	// the join.
	var viperblockKeyPath string
	if statusResp.ViperblockKey == "" {
		fmt.Println("⚠️  Leader did not provide a viperblock encryption key; at-rest encryption disabled on this node")
	} else {
		viperblockKeyBytes, err := base64.StdEncoding.DecodeString(statusResp.ViperblockKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error decoding viperblock encryption key: %v\n", err)
			os.Exit(1)
		}
		viperblockKeyPath, err = saveViperblockEncryptionKey(configDir, viperblockKeyBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error saving viperblock encryption key: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Viperblock encryption key received from leader: %s\n", viperblockKeyPath)
	}

	// Generate server cert signed by CA with this node's bind IP
	if err := admin.GenerateServerCertOnly(configDir, bindIP, region, config.DefaultAWSInternalSuffix); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating server certificate: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Server certificate generated with bind IP: %s\n\n", bindIP)

	// Match the leader's intra-AZ IPsec posture: NetworkConfig.IPSecEnabled is
	// authoritative. When the formation response omits NetworkConfig entirely
	// (no current code path; defensive guard against future regressions) fall
	// back to the AWS-parity default of ON.
	ipsecEnabled := true
	if statusResp.NetworkConfig != nil {
		ipsecEnabled = statusResp.NetworkConfig.IPSecEnabled
	}
	if ipsecEnabled {
		caCertPath := filepath.Join(configDir, "ca.pem")
		caKeyPath := filepath.Join(configDir, "ca.key")
		if err := admin.GenerateIPSecPeerCert(configDir, caCertPath, caKeyPath, node, bindIP); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating IPsec peer certificate: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("🔐 IPsec peer certificate generated (intra-AZ Geneve encryption ON)")
	}

	// Build cluster topology from formation data
	clusterRoutes := formation.BuildClusterRoutes(statusResp.Nodes)
	predastoreNodes := formation.BuildPredastoreNodes(statusResp.Nodes)
	ovnNBAddr, ovnSBAddr := formation.BuildOVNDBAddrs(statusResp.Nodes)

	fmt.Println("📝 Creating configuration files...")

	dirs, err := createConfigSubdirs(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config subdirectories: %v\n", err)
		os.Exit(1)
	}

	portStr := strconv.Itoa(port)

	northstarCreds, northstarConfigPath := northstarFromFormation(creds, dirs)

	// Generate multi-node predastore config
	var predastoreNodeID int
	hasPredastoreConfig := len(predastoreNodes) >= 2

	if hasPredastoreConfig {
		predastoreContent, err := admin.GenerateMultiNodePredastoreConfig(predastoreMultiNodeTemplate, predastoreNodes, creds.AccessKey, creds.SecretKey, creds.Region, creds.NatsToken, configDir, bindIP, compactionInterval,
			northstarCreds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating multi-node predastore config: %v\n", err)
			os.Exit(1)
		}

		predastorePath := filepath.Join(dirs.Predastore, "predastore.toml")
		if err := os.WriteFile(predastorePath, []byte(predastoreContent), 0600); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing predastore config: %v\n", err)
			os.Exit(1)
		}

		predastoreNodeID = admin.FindNodeIDByIP(predastoreNodes, bindIP)
		if predastoreNodeID == 0 {
			fmt.Fprintf(os.Stderr, "❌ Error: bind IP %s not found in predastore node list\n", bindIP)
			os.Exit(1)
		}
		fmt.Printf("✅ Created: multi-node predastore.toml (node ID: %d)\n", predastoreNodeID)
	}

	spinifexTomlPath := filepath.Join(configDir, "spinifex.toml")

	configSettings := admin.ConfigSettings{
		AccessKey: creds.AccessKey,
		SecretKey: creds.SecretKey,
		AccountID: creds.AccountID,
		Region:    creds.Region,
		NatsToken: creds.NatsToken,
		DataDir:   dataDir,
		LogDir:    LogDirFor(dataDir),
		ConfigDir: configDir,

		Node:          node,
		Az:            az,
		Port:          portStr,
		BindIP:        bindIP,
		AdvertiseIP:   advertiseIP,
		ClusterBindIP: clusterBind,
		ClusterRoutes: clusterRoutes,
		ClusterName:   creds.ClusterName,

		PredastoreNodeID:          predastoreNodeID,
		CompactionIntervalSeconds: compactionInterval,
		Services:                  services,
		RemoteNodes:               buildRemoteNodes(statusResp.Nodes, node, northstarConfigPath),

		OperatorEmail: email,

		EncryptionKeyFile: viperblockKeyPath,

		NorthstarAccessKey:      northstarCreds.AccessKey,
		NorthstarSecretKey:      northstarCreds.SecretKey,
		NorthstarBucket:         northstarCreds.Bucket,
		NorthstarDefaultDomain:  admin.NorthstarDefaultDomain,
		NorthstarInternalDomain: admin.NorthstarInternalDomain,
		NorthstarConfigPath:     northstarConfigPath,

		// Multi-endpoint OVN NB/SB list across the RAFT quorum so the client
		// fails over instead of pinning to a single init node.
		OVNNBAddr: ovnNBAddr,
		OVNSBAddr: ovnSBAddr,
	}

	if statusResp.NetworkConfig != nil {
		applyNetworkConfig(&configSettings, statusResp.NetworkConfig)
	}

	if err := generateAndWriteConfigs(dirs, spinifexTomlPath, configSettings, hasPredastoreConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating configuration files: %v\n", err)
		os.Exit(1)
	}

	finalizeNodeSetup(dataDir, caCertPath, creds.AdminAccessKey, creds.AdminSecretKey, creds.Region, bindIP)

	// Print cluster summary
	fmt.Println("\n🎉 Node successfully joined cluster!")
	fmt.Printf("   Cluster: %s (%d nodes)\n", creds.ClusterName, len(statusResp.Nodes))
	fmt.Printf("   Bind: %s  Advertise: %s  Loopback: 127.0.0.1\n", bindIP, advertiseIP)
	fmt.Println("   Nodes:")
	for name, n := range statusResp.Nodes {
		adv := n.AdvertiseIP
		if adv == "" {
			adv = n.BindIP
		}
		fmt.Printf("     - %s (bind=%s advertise=%s)\n", name, n.BindIP, adv)
	}
}

// resolveAdvertiseIP picks the off-host dial target for this node.
// Precedence: explicit --advertise flag > specific --bind IP > auto-detected
// WAN IP > loopback (with warning that off-host clients cannot reach this node).
func resolveAdvertiseIP(bindIP, advertiseFlag string, detected *admin.DetectedNetwork) (string, error) {
	if advertiseFlag != "" {
		if net.ParseIP(advertiseFlag) == nil {
			return "", fmt.Errorf("--advertise: invalid IP %q", advertiseFlag)
		}
		return advertiseFlag, nil
	}
	if bindIP != "" && bindIP != "0.0.0.0" && bindIP != "127.0.0.1" {
		return bindIP, nil
	}
	if detected != nil && detected.WAN != nil && detected.WAN.IP != "" {
		return detected.WAN.IP, nil
	}
	fmt.Fprintln(os.Stderr,
		"⚠️  Could not auto-detect a WAN IP. Off-host clients (ALB VMs, remote operators) "+
			"will not be able to reach this node. Re-run with --advertise <IP> to fix.")
	return "127.0.0.1", nil
}

// buildRemoteNodes converts formation NodeInfo into RemoteNode entries,
// excluding the local node. This puts all cluster members into spinifex.toml
// so config is the source of truth for expected cluster membership.
//
// northstarConfigPath is the local node's own path, republished for every peer:
// every node in a formed cluster runs northstar, and the seed set must be
// identical on all of them or the base zone's NS records get pinned to whichever
// node wins the create-if-absent race. --config-dir is per-node and does not
// cross the wire, so the value is accurate whenever nodes share a config dir and
// inert when they do not — only its emptiness is ever read back. Passing it
// empty (no credentials distributed) leaves peers with no stanza, so no node is
// advertised as a resolver it cannot be.
func buildRemoteNodes(allNodes map[string]formation.NodeInfo, localNode, northstarConfigPath string) []admin.RemoteNode {
	var remote []admin.RemoteNode
	for name, n := range allNodes {
		if name == localNode {
			continue
		}
		// Prefer the peer's advertise IP (off-host dial target); fall back
		// to BindIP for pre-siv-8 joiners that didn't send AdvertiseIP.
		host := n.AdvertiseIP
		if host == "" {
			host = n.BindIP
		}
		remote = append(remote, admin.RemoteNode{
			Name:                name,
			Host:                host,
			Region:              n.Region,
			AZ:                  n.AZ,
			Services:            n.Services,
			NorthstarConfigPath: northstarConfigPath,
		})
	}
	sort.Slice(remote, func(i, j int) bool {
		return remote[i].Name < remote[j].Name
	})
	return remote
}

// initIAMServiceFromConfig loads config, connects to NATS, loads the master
// key, and returns an initialised IAMServiceImpl. Callers must defer nc.Close().
func initIAMServiceFromConfig() (*handlers_iam.IAMServiceImpl, *config.ClusterConfig, *nats.Conn, func(), error) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("connect to cluster: %w", err)
	}

	masterKeyPath := filepath.Join(cfg.NodeBaseDir(), "config", "master.key")
	masterKey, err := handlers_iam.LoadMasterKey(masterKeyPath)
	if err != nil {
		nc.Close()
		return nil, nil, nil, nil, fmt.Errorf("load master key: %w", err)
	}

	svc, err := handlers_iam.NewIAMServiceImpl(nc, masterKey, len(cfg.Nodes))
	if err != nil {
		nc.Close()
		return nil, nil, nil, nil, fmt.Errorf("init IAM service: %w", err)
	}

	return svc, cfg, nc, func() { nc.Close() }, nil
}

// adminAccessPolicyDocument is the AdministratorAccess policy document that
// grants full access to all actions and resources.
const adminAccessPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

func runAccountCreate(cmd *cobra.Command, args []string) {
	name, _ := cmd.Flags().GetString("name")

	svc, cfg, nc, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	// 1. Create the account
	account, err := svc.CreateAccount(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating account: %v\n", err)
		os.Exit(1)
	}
	accountID := account.AccountID

	// Create default VPC for the new account (belt-and-suspenders: daemon also
	// does this via iam.account.created event, but daemon may not be running).
	nodeConfig := cfg.Nodes[cfg.Node]
	vpcSvc, vpcErr := handlers_ec2_vpc.NewVPCServiceImplWithNATS(&nodeConfig, nc)
	if vpcErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create default VPC service: %v\n", vpcErr)
	} else if _, vpcErr = vpcSvc.EnsureDefaultVPC(accountID); vpcErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create default VPC: %v\n", vpcErr)
	}

	// 2. Create admin user
	_, err = svc.CreateUser(accountID, &iam.CreateUserInput{
		UserName: aws.String("admin"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating admin user: %v\n", err)
		os.Exit(1)
	}

	// 3. Create access key for admin user
	akOut, err := svc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("admin"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating access key: %v\n", err)
		os.Exit(1)
	}

	// 4. Create AdministratorAccess policy scoped to this account
	policyARN := fmt.Sprintf("arn:aws:iam::%s:policy/AdministratorAccess", accountID)
	_, err = svc.CreatePolicy(accountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("AdministratorAccess"),
		PolicyDocument: aws.String(adminAccessPolicyDocument),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating admin policy: %v\n", err)
		os.Exit(1)
	}

	// 5. Attach policy to admin user
	_, err = svc.AttachUserPolicy(accountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("admin"),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error attaching policy: %v\n", err)
		os.Exit(1)
	}

	// Configure AWS CLI profile automatically
	profileName := "spinifex-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	homeDir, _ := os.UserHomeDir()

	endpointHost := "localhost"
	certPath := filepath.Join(cfg.NodeBaseDir(), "config", "ca.pem")
	nodeConfig = cfg.Nodes[cfg.Node]
	if h, _, err := net.SplitHostPort(nodeConfig.AWSGW.Host); err == nil {
		if h != "" && h != "0.0.0.0" {
			endpointHost = h
		}
	}
	endpointURL := "https://" + net.JoinHostPort(endpointHost, "9999")

	credPath := filepath.Join(homeDir, ".aws", "credentials")
	configPath := filepath.Join(homeDir, ".aws", "config")

	if err := admin.UpdateAWSINIFile(credPath, profileName, map[string]string{
		"aws_access_key_id":     *akOut.AccessKey.AccessKeyId,
		"aws_secret_access_key": *akOut.AccessKey.SecretAccessKey,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not update AWS credentials: %v\n", err)
	}
	region := cfg.Nodes[cfg.Node].Region
	if region == "" {
		region = "ap-southeast-2"
	}
	if err := admin.UpdateAWSINIFile(configPath, "profile "+profileName, map[string]string{
		"region":       region,
		"endpoint_url": endpointURL,
		"ca_bundle":    certPath,
		"output":       "json",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not update AWS config: %v\n", err)
	}

	// Print credentials
	fmt.Println("\nAccount created successfully!")
	fmt.Printf("  Account ID:        %s\n", accountID)
	fmt.Printf("  Account Name:      %s\n", name)
	fmt.Printf("  Admin User:        admin\n")
	fmt.Printf("  Access Key ID:     %s\n", *akOut.AccessKey.AccessKeyId)
	fmt.Printf("  Secret Access Key: %s\n", *akOut.AccessKey.SecretAccessKey)
	fmt.Printf("  AWS Profile:       %s\n", profileName)
	fmt.Println("\nUse with:")
	fmt.Printf("  AWS_PROFILE=%s aws ec2 describe-instances\n", profileName)
}

func runAccountList(cmd *cobra.Command, args []string) {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	accounts, err := svc.ListAccounts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing accounts: %v\n", err)
		os.Exit(1)
	}

	if len(accounts) == 0 {
		fmt.Println("No accounts found.")
		return
	}

	fmt.Printf("%-14s %-20s %-10s %s\n", "ACCOUNT ID", "NAME", "STATUS", "CREATED")
	fmt.Printf("%-14s %-20s %-10s %s\n", "----------", "----", "------", "-------")
	for _, a := range accounts {
		created := a.CreatedAt
		if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
			created = t.Format("2006-01-02 15:04")
		}
		fmt.Printf("%-14s %-20s %-10s %s\n", a.AccountID, a.AccountName, a.Status, created)
	}
}

func runCertRenew(cmd *cobra.Command, _ []string) {
	configDir, _ := cmd.Root().Flags().GetString("config-dir")

	// Verify CA files exist.
	caCertPath := filepath.Join(configDir, "ca.pem")
	caKeyPath := filepath.Join(configDir, "ca.key")
	if !admin.FileExists(caCertPath) || !admin.FileExists(caKeyPath) {
		fmt.Fprintf(os.Stderr, "Error: CA files not found in %s\nRun 'spx admin init' first.\n", configDir)
		os.Exit(1)
	}

	extraIPs, _ := cmd.Flags().GetStringSlice("extra-ip")
	extraDNS, _ := cmd.Flags().GetStringSlice("extra-dns")

	serverCertPath := filepath.Join(configDir, "server.pem")
	serverKeyPath := filepath.Join(configDir, "server.key")

	if err := admin.GenerateSignedCert(serverCertPath, serverKeyPath, caCertPath, caKeyPath, extraIPs, extraDNS); err != nil {
		fmt.Fprintf(os.Stderr, "Error regenerating server certificate: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Server certificate regenerated with current IPs and hostname")
	fmt.Printf("   Certificate: %s\n", serverCertPath)
	fmt.Println("\n⚠️  Restart awsgw and daemon services to pick up the new certificate.")
}

// configDirs holds the paths to config subdirectories created by createConfigSubdirs.
type configDirs struct {
	AWSGW      string
	Predastore string
	Viperblock string
	NATS       string
	Spinifex   string
	Northstar  string
}

// createConfigSubdirs creates the standard config subdirectories under configDir.
func createConfigSubdirs(configDir string) (configDirs, error) {
	dirs := configDirs{
		AWSGW:      filepath.Join(configDir, "awsgw"),
		Predastore: filepath.Join(configDir, "predastore"),
		Viperblock: filepath.Join(configDir, "viperblock"),
		NATS:       filepath.Join(configDir, "nats"),
		Spinifex:   filepath.Join(configDir, "spinifex"),
		Northstar:  filepath.Join(configDir, "northstar"),
	}
	for _, dir := range []string{dirs.AWSGW, dirs.Predastore, dirs.Viperblock, dirs.NATS, dirs.Spinifex, dirs.Northstar} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return configDirs{}, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return dirs, nil
}

// northstarFromFormation derives a joining node's northstar credentials and
// node-local config path from the cluster-wide pair distributed at formation.
//
// The keys cross the wire because a node's predastore only honours the keys in
// its own config, so every node must present the same pair to read the
// distributed zone bucket. The bucket name and config path are node-local
// constants, so they are derived here rather than carried.
//
// A leader that predates credential distribution sends no keys. That yields a
// zero pair and an empty path, so the node renders no northstar config at all
// rather than a resolver holding a key its own predastore would reject.
func northstarFromFormation(creds *formation.SharedCredentials, dirs configDirs) (admin.NorthstarCredentials, string) {
	if creds.NorthstarAccessKey == "" || creds.NorthstarSecretKey == "" {
		return admin.NorthstarCredentials{}, ""
	}
	return admin.NorthstarCredentials{
		AccessKey: creds.NorthstarAccessKey,
		SecretKey: creds.NorthstarSecretKey,
		Bucket:    admin.NorthstarBucketName,
	}, filepath.Join(dirs.Northstar, "northstar.toml")
}

// generateAndWriteConfigs renders the standard config files (spinifex.toml,
// awsgw.toml, nats.conf, and optionally predastore.toml) from templates.
func generateAndWriteConfigs(dirs configDirs, spinifexTomlPath string, settings admin.ConfigSettings, skipPredastore bool) error {
	// A one-sided pair must disable Northstar wholesale. Rendering only the
	// public stanza or only the secret file advertises a resolver that cannot run.
	northstarEnabled := settings.NorthstarAccessKey != "" && settings.NorthstarSecretKey != ""
	if !northstarEnabled {
		settings.NorthstarAccessKey = ""
		settings.NorthstarSecretKey = ""
		settings.NorthstarBucket = ""
		settings.NorthstarConfigPath = ""
	}

	configs := []admin.ConfigFile{
		{Name: "spinifex.toml", Path: spinifexTomlPath, Template: spinifexTomlTemplate},
		{Name: filepath.Join(dirs.AWSGW, "awsgw.toml"), Path: filepath.Join(dirs.AWSGW, "awsgw.toml"), Template: awsgwTomlTemplate},
		{Name: filepath.Join(dirs.NATS, "nats.conf"), Path: filepath.Join(dirs.NATS, "nats.conf"), Template: natsConfTemplate},
	}
	// northstar.toml is only rendered when scoped DNS credentials were
	// provisioned. A cluster formed by a leader that predates their distribution
	// leaves the keys empty, yielding no northstar config rather than a partial
	// one pointing at a bucket the local predastore does not serve.
	if northstarEnabled {
		configs = append(configs, admin.ConfigFile{
			Name: filepath.Join(dirs.Northstar, "northstar.toml"), Path: filepath.Join(dirs.Northstar, "northstar.toml"), Template: northstarTomlTemplate,
		})
	}
	if !skipPredastore {
		configs = append(configs, admin.ConfigFile{
			Name: filepath.Join(dirs.Predastore, "predastore.toml"), Path: filepath.Join(dirs.Predastore, "predastore.toml"), Template: predastoreTomlTemplate,
		})
	}
	return admin.GenerateConfigFiles(configs, settings)
}

// finalizeNodeSetup configures AWS credentials, creates service directories,
// and sets ownership when running as root.
func finalizeNodeSetup(dataDir, certPath, adminAccessKey, adminSecretKey, region, bindIP string) {
	fmt.Println("\n🔧 Configuring AWS credentials...")
	if err := admin.SetupAWSCredentials(adminAccessKey, adminSecretKey, region, certPath, bindIP); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not update AWS credentials: %v\n", err)
	} else {
		fmt.Println("✅ AWS credentials configured")
	}

	admin.CreateServiceDirectories(dataDir)

	if os.Getuid() == 0 {
		admin.SetServiceOwnership()
	}
}

// applyNetworkConfig copies cluster-wide network settings from a formation
// NetworkConfig into ConfigSettings and auto-detects the local WAN interface.
func applyNetworkConfig(settings *admin.ConfigSettings, nc *formation.NetworkConfig) {
	settings.IPSecEnabled = nc.IPSecEnabled
	settings.ExternalMode = nc.ExternalMode
	settings.PoolDNSServers = nc.PoolDNSServers
	if nc.ExternalMode != "" {
		settings.Pools = []admin.PoolData{{
			Name:       nc.PoolName,
			Source:     nc.PoolSource,
			BindBridge: nc.PoolBindBridge,
			Start:      nc.PoolStart,
			End:        nc.PoolEnd,
			Gateway:    nc.PoolGateway,
			GatewayIP:  nc.PoolGatewayIP,
			PrefixLen:  nc.PoolPrefixLen,
			DNSServers: nc.PoolDNSServers,
		}}
	}

	settings.BootstrapAccountId = nc.BootstrapAccountId
	settings.BootstrapVpcId = nc.BootstrapVpcId
	settings.BootstrapSubnetId = nc.BootstrapSubnetId
	settings.BootstrapIgwId = nc.BootstrapIgwId
	settings.BootstrapCidr = nc.BootstrapCidr
	settings.BootstrapSubnetCidr = nc.BootstrapSubnetCidr

	if nc.ExternalMode != "" {
		detected, err := admin.DetectNetwork()
		if err == nil && detected.WAN != nil {
			settings.ExternalIface = detected.WAN.Name
		}
	}
}

// writeBootstrapResult holds the admin credentials so callers can
// write them to ~/.aws/credentials instead of the system credentials.
type writeBootstrapResult struct {
	AdminAccessKey string
	AdminSecretKey string
}

// ensureMasterKey load-or-generates the IAM master key at <configDir>/master.key.
// It returns the key bytes and whether the key already existed on disk. On a
// --force re-init the existing key is preserved: rotating it would orphan every
// IAM secret in NATS KV (e.g. the ECR signing key) that was encrypted under it.
func ensureMasterKey(configDir string) (key []byte, existed bool, err error) {
	keyPath := filepath.Join(configDir, "master.key")
	if admin.FileExists(keyPath) {
		key, err = handlers_iam.LoadMasterKey(keyPath)
		if err != nil {
			return nil, false, fmt.Errorf("load master key: %w", err)
		}
		return key, true, nil
	}
	key, err = handlers_iam.GenerateMasterKey()
	if err != nil {
		return nil, false, fmt.Errorf("generate master key: %w", err)
	}
	return key, false, nil
}

// loadSystemCredentials reads the preserved system access/secret key from
// <configDir>/system-credentials.json. On a re-init the configs must render the
// same system credentials that seeded the NATS KV `system` secret, else SigV4
// auth between services fails.
func loadSystemCredentials(configDir string) (accessKey, secretKey string, err error) {
	data, err := os.ReadFile(filepath.Join(configDir, "system-credentials.json"))
	if err != nil {
		return "", "", fmt.Errorf("read system credentials: %w", err)
	}
	var creds struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", fmt.Errorf("parse system credentials: %w", err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return "", "", fmt.Errorf("system credentials missing access/secret key")
	}
	return creds.AccessKeyID, creds.SecretAccessKey, nil
}

// writeBootstrapFiles generates new admin credentials and writes the bootstrap
// files (master.key to configDir, bootstrap.json to bootstrapDir).
// Used by init flows (single and multi-node).
func writeBootstrapFiles(configDir, bootstrapDir string, masterKey []byte, accessKey, secretKey, accountID string) (*writeBootstrapResult, error) {
	adminAccessKey, err := admin.GenerateAWSAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate admin access key: %w", err)
	}
	adminSecretKey, err := admin.GenerateAWSSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate admin secret key: %w", err)
	}
	if err := writeBootstrapFilesWithAdmin(configDir, bootstrapDir, masterKey, accessKey, secretKey, accountID, adminAccessKey, adminSecretKey); err != nil {
		return nil, err
	}
	return &writeBootstrapResult{
		AdminAccessKey: adminAccessKey,
		AdminSecretKey: adminSecretKey,
	}, nil
}

// writePredastoreEncryptionKey load-or-generates this node's per-node predastore
// key at <configDir>/predastore/encryption.key (mode 0600). The key is per-node:
// every node generates its own at init/join time and never transmits it, because
// predastore only opens fragments on the node that sealed them. An existing key
// is preserved so a --force re-init does not orphan already-sealed fragments.
func writePredastoreEncryptionKey(configDir string) (string, error) {
	predastoreDir := filepath.Join(configDir, "predastore")
	if err := os.MkdirAll(predastoreDir, 0750); err != nil {
		return "", fmt.Errorf("create predastore config dir: %w", err)
	}
	keyPath := filepath.Join(predastoreDir, "encryption.key")
	if admin.FileExists(keyPath) {
		return keyPath, nil
	}
	key, err := handlers_iam.GenerateMasterKey()
	if err != nil {
		return "", fmt.Errorf("generate predastore encryption key: %w", err)
	}
	if err := handlers_iam.SaveMasterKey(keyPath, key); err != nil {
		return "", fmt.Errorf("save predastore encryption key: %w", err)
	}
	return keyPath, nil
}

// ensureViperblockEncryptionKey load-or-generates the cluster-wide viperblock
// at-rest key at <configDir>/viperblock/encryption.key (mode 0600) and returns
// its bytes and path. Unlike the predastore key it is shared: the leader loads
// these bytes to distribute the same key to joiners via the formation server, so
// a volume sealed on any node can be opened on any other. An existing key is
// preserved so a --force re-init does not orphan already-encrypted volumes.
func ensureViperblockEncryptionKey(configDir string) ([]byte, string, error) {
	viperblockDir := filepath.Join(configDir, "viperblock")
	if err := os.MkdirAll(viperblockDir, 0750); err != nil {
		return nil, "", fmt.Errorf("create viperblock config dir: %w", err)
	}
	keyPath := filepath.Join(viperblockDir, "encryption.key")
	if admin.FileExists(keyPath) {
		key, err := handlers_iam.LoadMasterKey(keyPath)
		if err != nil {
			return nil, "", fmt.Errorf("load viperblock encryption key: %w", err)
		}
		return key, keyPath, nil
	}
	key, err := handlers_iam.GenerateMasterKey()
	if err != nil {
		return nil, "", fmt.Errorf("generate viperblock encryption key: %w", err)
	}
	if err := handlers_iam.SaveMasterKey(keyPath, key); err != nil {
		return nil, "", fmt.Errorf("save viperblock encryption key: %w", err)
	}
	return key, keyPath, nil
}

// saveViperblockEncryptionKey writes an already-generated 32-byte viperblock
// master key (received from the formation leader) to
// <configDir>/viperblock/encryption.key with mode 0600.
func saveViperblockEncryptionKey(configDir string, key []byte) (string, error) {
	viperblockDir := filepath.Join(configDir, "viperblock")
	if err := os.MkdirAll(viperblockDir, 0750); err != nil {
		return "", fmt.Errorf("create viperblock config dir: %w", err)
	}
	keyPath := filepath.Join(viperblockDir, "encryption.key")
	if err := handlers_iam.SaveMasterKey(keyPath, key); err != nil {
		return "", fmt.Errorf("save viperblock encryption key: %w", err)
	}
	return keyPath, nil
}

// writeSystemCredentials writes the system access key to a plaintext JSON file.
// The daemon reads this at startup to inject credentials into ALB VM cloud-init
// for SigV4-authenticated communication with the AWS gateway.
func writeSystemCredentials(configDir, accessKey, secretKey string) error {
	creds := struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}{
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling system credentials: %w", err)
	}
	return os.WriteFile(filepath.Join(configDir, "system-credentials.json"), data, 0600)
}

// writeBootstrapFilesWithAdmin writes the bootstrap files using the provided
// admin credentials. master.key goes to configDir, bootstrap.json goes to
// bootstrapDir (the awsgw data directory) so it stays outside /etc/spinifex.
func writeBootstrapFilesWithAdmin(configDir, bootstrapDir string, masterKey []byte, accessKey, secretKey, accountID, adminAccessKey, adminSecretKey string) error {
	if err := os.MkdirAll(bootstrapDir, 0700); err != nil {
		return fmt.Errorf("create bootstrap directory %s: %w", bootstrapDir, err)
	}
	if err := handlers_iam.SaveMasterKey(filepath.Join(configDir, "master.key"), masterKey); err != nil {
		return fmt.Errorf("saving master key: %w", err)
	}
	encryptedSecret, err := handlers_iam.EncryptSecret(secretKey, masterKey)
	if err != nil {
		return fmt.Errorf("encrypting system secret: %w", err)
	}

	adminEncryptedSecret, err := handlers_iam.EncryptSecret(adminSecretKey, masterKey)
	if err != nil {
		return fmt.Errorf("encrypting admin secret: %w", err)
	}

	bd := &handlers_iam.BootstrapData{
		Version:         handlers_iam.BootstrapVersion,
		AccessKeyID:     accessKey,
		EncryptedSecret: encryptedSecret,
		AccountID:       accountID,
		Admin: &handlers_iam.AdminBootstrapData{
			AccountID:       admin.DefaultAccountID(),
			AccountName:     admin.DefaultAccountName(),
			UserName:        "admin",
			AccessKeyID:     adminAccessKey,
			EncryptedSecret: adminEncryptedSecret,
		},
	}

	return handlers_iam.SaveBootstrapData(filepath.Join(bootstrapDir, "bootstrap.json"), bd)
}

// isNonBridgeableUplink reports whether the interface name indicates an uplink
// that cannot be enslaved to an L2 bridge: WiFi (wl*), cellular (ww*), PPP.
func isNonBridgeableUplink(name string) bool {
	return strings.HasPrefix(name, "wl") || strings.HasPrefix(name, "ww") || strings.HasPrefix(name, "ppp")
}

// resolvePublicPoolFlags validates the public-pool flag set shared by pool
// mode and nat-with-public-pool. Returns the resolved source, static range
// start/end, and bind bridge. Source defaults to dhcp when no range is given;
// defaultBindBridge fills --external-bind-bridge for dhcp (br-wan in pool
// mode, the uplink interface in nat mode).
func resolvePublicPoolFlags(source, poolRange, bindBridge, gateway, defaultBindBridge string) (resolvedSource, start, end, resolvedBindBridge string, err error) {
	if source == "" {
		if poolRange == "" {
			source = "dhcp"
		} else {
			source = "static"
		}
	}
	switch source {
	case "dhcp":
		if poolRange != "" {
			return "", "", "", "", fmt.Errorf("--external-pool not allowed with --external-source=dhcp (addresses come from upstream DHCP server)")
		}
		if bindBridge == "" {
			if defaultBindBridge == "" {
				return "", "", "", "", fmt.Errorf("--external-bind-bridge is required with --external-source=dhcp (no uplink interface detected)")
			}
			bindBridge = defaultBindBridge
		}
	case "static":
		if bindBridge != "" {
			return "", "", "", "", fmt.Errorf("--external-bind-bridge only valid with --external-source=dhcp")
		}
		if poolRange == "" {
			return "", "", "", "", fmt.Errorf("--external-pool is required with --external-source=static (e.g., 192.168.1.150-192.168.1.250)")
		}
		if gateway == "" {
			return "", "", "", "", fmt.Errorf("--external-gateway is required with --external-source=static")
		}
		parts := strings.SplitN(poolRange, "-", 2)
		if len(parts) != 2 || net.ParseIP(parts[0]) == nil || net.ParseIP(parts[1]) == nil {
			return "", "", "", "", fmt.Errorf("--external-pool must be start-end IPs (e.g., 192.168.1.150-192.168.1.250), got: %s", poolRange)
		}
		start, end = parts[0], parts[1]
	default:
		return "", "", "", "", fmt.Errorf("--external-source must be 'static' or 'dhcp', got: %s", source)
	}
	return source, start, end, bindBridge, nil
}

// bridgeModeFor returns the vpcd bridge_mode admin init persists for the
// external mode: only nat mode is pinned; bridged modes stay auto-detected.
func bridgeModeFor(externalMode string) string {
	if externalMode == "nat" {
		return "nat"
	}
	return ""
}

// detectDNSServers auto-detects DNS servers from the host for the specified
// interface. Uses resolvectl (systemd-resolved) first, then falls back to
// /etc/resolv.conf. Returns up to 3 servers. Falls back to public DNS if none found.
func detectDNSServers(iface string) []string {
	// Try resolvectl for the specific link first (most reliable on modern systems)
	if iface != "" {
		out, err := exec.Command("resolvectl", "dns", iface).CombinedOutput()
		if err == nil {
			servers := parseDNSFromResolvectl(string(out))
			if len(servers) > 0 {
				return servers
			}
		}
	}

	// Try resolvectl global
	out, err := exec.Command("resolvectl", "dns").CombinedOutput()
	if err == nil {
		servers := parseDNSFromResolvectl(string(out))
		if len(servers) > 0 {
			return servers
		}
	}

	// Fall back to /etc/resolv.conf
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var servers []string
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver ") {
				ip := strings.TrimSpace(strings.TrimPrefix(line, "nameserver"))
				// Skip localhost (systemd-resolved stub)
				if ip != "127.0.0.53" && ip != "127.0.0.1" && net.ParseIP(ip) != nil {
					servers = append(servers, ip)
				}
			}
		}
		if len(servers) > 0 {
			if len(servers) > 3 {
				servers = servers[:3]
			}
			return servers
		}
	}

	// Fallback to well-known public DNS
	return []string{"8.8.8.8", "1.1.1.1"}
}

// parseDNSFromResolvectl extracts IP addresses from resolvectl dns output.
// Format: "Link 2 (enp0s3): 192.168.1.1 8.8.8.8 1.1.1.1".
func parseDNSFromResolvectl(output string) []string {
	var servers []string
	for line := range strings.SplitSeq(output, "\n") {
		// Find the colon separator, IPs come after it
		_, after, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.FieldsSeq(after)
		for f := range fields {
			if net.ParseIP(f) != nil && f != "127.0.0.53" && f != "127.0.0.1" {
				servers = append(servers, f)
			}
		}
	}
	if len(servers) > 3 {
		servers = servers[:3]
	}
	return servers
}

// installCACertificate copies the Spinifex CA certificate into the system
// trust store and runs update-ca-certificates so TLS clients (AWS CLI, etc.)
// trust the self-signed gateway certificate without extra configuration.
func installCACertificate(caPemPath string) {
	if os.Getuid() != 0 {
		return
	}

	const systemCertPath = "/usr/local/share/ca-certificates/spinifex-ca.crt"

	data, err := os.ReadFile(caPemPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read CA certificate %s: %v\n", caPemPath, err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(systemCertPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create certificate directory: %v\n", err)
		return
	}

	if err := os.WriteFile(systemCertPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not install CA certificate: %v\n", err)
		return
	}

	cmd := exec.Command("update-ca-certificates")
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: update-ca-certificates failed: %v\n", err)
		return
	}

	fmt.Printf("✅ CA certificate installed to system trust store\n")
}

// runAdminBanner writes the Spinifex console banner to /etc/motd.
// With --boot-check it also detects management IP changes and updates node.conf.
// All errors are logged as warnings — the command always exits 0 so a banner
// failure never blocks the boot sequence.
func runAdminBanner(cmd *cobra.Command, _ []string) {
	bootCheck, _ := cmd.Flags().GetBool("boot-check")

	const nodeConf = "/etc/spinifex/node.conf"

	// Parse /etc/spinifex/node.conf (KEY=VALUE shell format).
	conf := parseNodeConf(nodeConf)
	iface := conf["MANAGEMENT_IFACE"]
	recordedIP := conf["MANAGEMENT_IP"]
	hostname := conf["NODE_HOSTNAME"]

	if hostname == "" {
		if h, err := os.Hostname(); err == nil {
			hostname = h
		}
	}

	// Resolve current IP from the management interface at runtime.
	// If node.conf is absent or has no MANAGEMENT_IFACE (source installs before
	// the first spx admin init writes node.conf), fall back to br-wan which
	// setup-ovn.sh always creates as the management bridge.
	if iface == "" {
		iface = "br-wan"
	}
	currentIP := resolveIfaceIP(iface)
	if currentIP == "" {
		currentIP = recordedIP // fall back to value recorded at install time
	}
	if currentIP == "" {
		currentIP = "<unknown>"
	}

	if bootCheck && iface != "" && recordedIP != "" && currentIP != recordedIP {
		slog.Info("Management IP changed", "old", recordedIP, "new", currentIP)
		conf["MANAGEMENT_IP"] = currentIP
		if err := writeNodeConf(nodeConf, conf); err != nil {
			slog.Warn("Failed to update node.conf with new IP", "err", err)
		} else {
			slog.Info("Updated node.conf with new management IP", "ip", currentIP)
		}
		// try-restart is safe even if the target isn't active yet.
		restartCmd := exec.Command("systemctl", "try-restart", "spinifex.target")
		restartCmd.Stdout = os.Stdout
		restartCmd.Stderr = os.Stderr
		if err := restartCmd.Run(); err != nil {
			// Services are still bound to the old IP — operator must act.
			slog.Error("Failed to restart spinifex.target after IP change — services may be unreachable on new IP", "err", err)
		}
	}

	banner := fmt.Sprintf(`
  +--------------------------------------------------------------+
  |          Spinifex  —  Mulga Defense Corporation              |
  +--------------------------------------------------------------+
  |  Node:      %-49s|
  |  Login:     %-49s|
  |  Dashboard: %-49s|
  |  API:       %-49s|
  |  SSH:       %-49s|
  +--------------------------------------------------------------+
  |  AWS credentials:  cat ~/.aws/credentials                    |
  +--------------------------------------------------------------+

`,
		hostname,
		"spinifex",
		"https://"+currentIP+":3000",
		"https://"+currentIP+":9999",
		"spinifex@"+currentIP,
	)

	banner += gpuBannerSection()

	// Write to /etc/issue — displayed on the console before the login prompt.
	// Overwrite entirely; this is a purpose-built appliance so we own this file.
	if err := os.WriteFile("/etc/issue", []byte(banner), 0o644); err != nil {
		slog.Warn("Failed to write /etc/issue", "err", err)
	}

	// Append to /etc/motd — displayed after SSH login, preserving any existing
	// content (e.g. the Debian disclaimer). A sentinel marks our section so
	// re-runs replace it cleanly rather than accumulating.
	if err := appendBannerToMotd(banner); err != nil {
		slog.Warn("Failed to write /etc/motd", "err", err)
	}
}

// appendBannerToMotd appends the Spinifex banner to /etc/motd, preserving any
// existing content. A sentinel line marks the start of the Spinifex section so
// repeated runs replace only our section rather than accumulating duplicates.
func appendBannerToMotd(banner string) error {
	const (
		motdPath = "/etc/motd"
		sentinel = "# --- Spinifex ---\n"
	)
	existing, _ := os.ReadFile(motdPath)
	base := string(existing)
	// Strip any previous Spinifex section from the sentinel onwards.
	if idx := strings.Index(base, sentinel); idx >= 0 {
		base = base[:idx]
	}
	// Ensure a blank line separates the existing content from our banner.
	if len(strings.TrimSpace(base)) > 0 && !strings.HasSuffix(base, "\n\n") {
		if strings.HasSuffix(base, "\n") {
			base += "\n"
		} else {
			base += "\n\n"
		}
	}
	return os.WriteFile(motdPath, []byte(base+sentinel+banner), 0o644)
}

// gpuBannerSection returns an optional banner box section describing GPU state.
// Returns "" when no GPU hardware is detected. Safe to call at boot before the
// daemon starts — all checks are sysfs/file reads, no NATS required.
func gpuBannerSection() string {
	devices, err := gpu.Discover()
	if err != nil || len(devices) == 0 {
		return ""
	}

	iommuEntries, _ := os.ReadDir("/sys/kernel/iommu_groups/")
	iommuActive := len(iommuEntries) > 0

	_, vfioErr := os.Stat("/sys/module/vfio_pci")
	vfioPresent := vfioErr == nil

	passthroughEnabled := false
	cfgPath := DefaultConfigFile()
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if nodeCfg, ok := cfg.Nodes[cfg.Node]; ok {
			passthroughEnabled = nodeCfg.Daemon.GPUPassthrough
		}
	}

	models := gpuModelSummary(devices)

	var statusLine, hintLine string
	switch {
	case passthroughEnabled:
		statusLine = "Passthrough enabled"
	case iommuActive && vfioPresent:
		statusLine = "Ready to enable"
		hintLine = "sudo spx admin gpu enable"
	default:
		statusLine = "Setup required"
		hintLine = "sudo spx admin gpu setup"
	}

	const (
		sep    = "  +--------------------------------------------------------------+\n"
		maxVal = 55
	)
	if len([]rune(models)) > maxVal {
		models = string([]rune(models)[:maxVal-3]) + "..."
	}

	section := sep +
		fmt.Sprintf("  |  GPU: %-55s|\n", models) +
		fmt.Sprintf("  |       %-55s|\n", statusLine)
	if hintLine != "" {
		section += fmt.Sprintf("  |       %-55s|\n", hintLine)
	}
	section += sep + "\n"
	return section
}

func gpuModelSummary(devices []gpu.GPUDevice) string {
	counts := make(map[string]int)
	var order []string
	for _, d := range devices {
		if counts[d.Model] == 0 {
			order = append(order, d.Model)
		}
		counts[d.Model]++
	}
	var parts []string
	for _, m := range order {
		if n := counts[m]; n > 1 {
			parts = append(parts, fmt.Sprintf("%dx %s", n, m))
		} else {
			parts = append(parts, m)
		}
	}
	return strings.Join(parts, ", ")
}

// parseNodeConf reads a KEY=VALUE shell-format file and returns a map.
// Lines starting with # and blank lines are ignored.
func parseNodeConf(path string) map[string]string {
	result := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("parseNodeConf: could not read node.conf", "path", path, "err", err)
		}
		return result
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		result[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return result
}

// writeNodeConf serialises a KEY=VALUE map back to the node.conf file.
// Only writes keys that were present in the original (preserves order via known keys).
func writeNodeConf(path string, conf map[string]string) error {
	// Write in a stable order matching what the installer creates.
	keys := []string{"MANAGEMENT_IP", "MANAGEMENT_IFACE", "NODE_HOSTNAME"}
	var b strings.Builder
	written := make(map[string]bool)
	for _, k := range keys {
		if v, ok := conf[k]; ok {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
			written[k] = true
		}
	}
	// Append any extra keys not in the known list.
	for k, v := range conf {
		if !written[k] {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// resolveIfaceIP returns the first IPv4 address assigned to iface, or "".
func resolveIfaceIP(iface string) string {
	if iface == "" {
		return ""
	}
	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		return ""
	}
	addrs, err := netIface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

// printChecksumError writes the failure, the source URL (printed for every
// error kind so 404/non-HTTPS/size-cap failures tell the operator which URL
// to investigate), and the exact --force recovery command. The cached file
// is left in place: an implicit auto-delete would mutate state inside
// "verify", and a tampered artifact is forensically useful intact.
func printChecksumError(w io.Writer, imageFile, imageName string, image utils.Images, err error) {
	fmt.Fprintf(w, "Image integrity verification failed: %v\n", err)
	fmt.Fprintf(w, "  file:     %s\n", imageFile)
	fmt.Fprintf(w, "  source:   %s\n", image.Checksum)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The cached file was left in place. To re-download and retry:")
	fmt.Fprintf(w, "  spx admin images import --name %s --force\n", imageName)
}
