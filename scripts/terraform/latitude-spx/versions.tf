terraform {
  required_version = ">= 1.5.0"
  required_providers {
    # Pinned exactly: 4.8.0 is the version whose schema these files were written against.
    latitudesh = { source = "latitudesh/latitudesh", version = "= 4.8.0" }
  }
}
