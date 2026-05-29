package handlers_imds

// MetaDataServerIP is the link-local address the EC2 Instance Metadata Service
// is served on, identical to AWS. Exported so the network topology layer can
// set it as options:arp_proxy on every subnet router LSP, making the address
// answer ARP link-local for any guest (DHCP or fully static) without hard-coding
// the literal in two places.
const MetaDataServerIP = "169.254.169.254"
