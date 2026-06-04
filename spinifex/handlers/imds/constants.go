package handlers_imds

// MetaDataServerIP is the link-local address the EC2 Instance Metadata Service is
// served on, identical to AWS. Exported so the network topology layer can set it as
// options:arp_proxy on every subnet router LSP, answering ARP for any guest.
const MetaDataServerIP = "169.254.169.254"
