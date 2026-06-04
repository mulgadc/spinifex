package handlers_imds

// MetaDataServerIP is the link-local address the EC2 Instance Metadata Service is
// served on, identical to AWS. Exported so the network topology layer can claim it
// on the subnet-switch localport that answers IMDS over L2 for every guest.
const MetaDataServerIP = "169.254.169.254"
