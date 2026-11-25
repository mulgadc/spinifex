### Validation

`RequestCertificate` ignores `ValidationMethod`, and `EMAIL` validation is not offered.
The platform selects the method from its configuration:

| Platform configuration | Validation | `ResourceRecord` |
| --- | --- | --- |
| DNS provider credential | The platform writes the record | None |
| Hosted zone, no provider credential | You publish a TXT record at `_acme-challenge.<domain>.` | TXT, not the CNAME that AWS returns |
| No public domain | Issued at once by a private CA | None |

A certificate validated by a manual TXT record is not renewed automatically.
