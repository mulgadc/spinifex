package ec2v1

// VolumeHeldElsewhereText is the text of a start refused because another node
// holds one of the instance's volumes. It crosses NATS as message text, so
// readers match on it; the provider that refuses and every reader share this
// string so they cannot drift.
const VolumeHeldElsewhereText = "volume is leased by another owner"
