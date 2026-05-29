package handlers_imds

// MetaDataServerIP is the link-local address the EC2 Instance Metadata Service
// is served on, identical to AWS. Exported so the network topology layer can
// push a /32 route to it via DHCP option 121 (RFC 3442) without hard-coding
// the literal in two places.
const MetaDataServerIP = "169.254.169.254"
