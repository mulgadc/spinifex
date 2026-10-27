import { describe, expect, it } from "vitest"

import {
  absoluteRecordName,
  createHostedZoneSchema,
  decodeRecordName,
  isZoneOwnedRecord,
  quoteTxt,
  type RecordSetFormData,
  recordSetSchema,
  relativeRecordName,
  toFormValues,
  toRecordSet,
  unquoteTxt,
} from "./route53"

const ZONE = "example.com."

describe("quoteTxt", () => {
  it("quotes a short value as one string", () => {
    expect(quoteTxt("v=spf1 -all")).toBe('"v=spf1 -all"')
  })

  it("escapes quotes and backslashes", () => {
    expect(quoteTxt('say "hi" \\o/')).toBe(String.raw`"say \"hi\" \\o/"`)
  })

  it("splits a value longer than 255 bytes into several strings", () => {
    const quoted = quoteTxt("a".repeat(300))

    expect(quoted).toBe(`"${"a".repeat(255)}" "${"a".repeat(45)}"`)
  })

  it("counts UTF-8 bytes, not characters, and never splits a character", () => {
    // é is two bytes, so 128 of them is 256 bytes: 127 fit in the first string.
    const quoted = quoteTxt("é".repeat(128))

    expect(quoted).toBe(`"${"é".repeat(127)}" "é"`)
  })
})

describe("unquoteTxt", () => {
  it("reverses quoteTxt, joining split strings", () => {
    const long = `${"x".repeat(260)} "quoted" \\`

    expect(unquoteTxt(quoteTxt(long))).toBe(long)
  })

  it("decodes decimal \\ddd escapes", () => {
    expect(unquoteTxt(String.raw`"a\059b"`)).toBe("a;b")
  })

  it("returns an unquoted value unchanged", () => {
    expect(unquoteTxt("  plain  ")).toBe("plain")
  })
})

describe("record names", () => {
  it("decodes Route 53's octal escapes", () => {
    expect(decodeRecordName(String.raw`\052.example.com.`)).toBe(
      "*.example.com.",
    )
  })

  it("makes a name relative to its zone, with the apex as empty", () => {
    expect(relativeRecordName("www.example.com.", ZONE)).toBe("www")
    expect(relativeRecordName("Example.COM.", ZONE)).toBe("")
    expect(relativeRecordName(String.raw`\052.example.com.`, ZONE)).toBe("*")
  })

  it("makes a relative name absolute", () => {
    expect(absoluteRecordName("www", "example.com")).toBe("www.example.com.")
    expect(absoluteRecordName("", ZONE)).toBe(ZONE)
  })
})

describe("isZoneOwnedRecord", () => {
  it("is true only for NS and SOA at the apex", () => {
    expect(isZoneOwnedRecord({ Name: ZONE, Type: "NS" }, ZONE)).toBeTruthy()
    expect(
      isZoneOwnedRecord({ Name: ZONE, Type: "SOA" }, "example.com"),
    ).toBeTruthy()
    expect(
      isZoneOwnedRecord({ Name: "sub.example.com.", Type: "NS" }, ZONE),
    ).toBeFalsy()
    expect(isZoneOwnedRecord({ Name: ZONE, Type: "TXT" }, ZONE)).toBeFalsy()
  })
})

describe("createHostedZoneSchema", () => {
  it("accepts a domain with or without the trailing dot", () => {
    expect(
      createHostedZoneSchema.safeParse({ name: "example.com" }).success,
    ).toBeTruthy()
    expect(
      createHostedZoneSchema.safeParse({ name: "a.example.com." }).success,
    ).toBeTruthy()
  })

  it("refuses a single label and invalid characters", () => {
    expect(
      createHostedZoneSchema.safeParse({ name: "localhost" }).success,
    ).toBeFalsy()
    expect(
      createHostedZoneSchema.safeParse({ name: "bad_name.com" }).success,
    ).toBeFalsy()
  })
})

function issues(input: Partial<RecordSetFormData>): string[] {
  const result = recordSetSchema.safeParse({
    name: "",
    type: "A",
    ttl: 300,
    values: "",
    ...input,
  })

  return result.success ? [] : result.error.issues.map((i) => i.message)
}

describe("recordSetSchema", () => {
  it("accepts valid values for each type", () => {
    expect(issues({ type: "A", values: "192.0.2.1\n192.0.2.2" })).toStrictEqual(
      [],
    )
    expect(issues({ type: "AAAA", values: "2001:db8::1" })).toStrictEqual([])
    expect(
      issues({ type: "CNAME", values: "target.example.net" }),
    ).toStrictEqual([])
    expect(issues({ type: "MX", values: "10 mail.example.com" })).toStrictEqual(
      [],
    )
    expect(issues({ type: "TXT", values: 'anything "goes"' })).toStrictEqual([])
    expect(
      issues({ type: "SRV", values: "10 5 5060 sip.example.com" }),
    ).toStrictEqual([])
    expect(
      issues({ type: "CAA", values: '0 issue "letsencrypt.org"' }),
    ).toStrictEqual([])
    expect(issues({ name: "*", values: "192.0.2.1" })).toStrictEqual([])
  })

  it("requires a value", () => {
    expect(issues({ values: "  \n " })).toStrictEqual([
      "At least one value is required",
    ])
  })

  it("names the value that does not fit the type", () => {
    expect(issues({ values: "192.0.2.1\n300.1.1.1" })).toStrictEqual([
      '"300.1.1.1" is not an IPv4 address',
    ])
    expect(issues({ type: "MX", values: "mail.example.com" })).toStrictEqual([
      '"mail.example.com" must be a priority and a mail server',
    ])
  })

  it("allows one CNAME value only", () => {
    expect(issues({ type: "CNAME", values: "a.example\nb.example" })).toContain(
      "A CNAME record takes exactly one value",
    )
  })

  it("refuses a malformed name and a negative TTL", () => {
    expect(issues({ name: "bad name", values: "192.0.2.1" })).toHaveLength(1)
    expect(issues({ ttl: -1, values: "192.0.2.1" })).toStrictEqual([
      "TTL must be at least 0",
    ])
  })
})

describe("form mapping", () => {
  it("quotes TXT lines on the way out and unquotes them on the way in", () => {
    const recordSet = toRecordSet(
      {
        name: "_dmarc",
        type: "TXT",
        ttl: 60,
        values: "v=DMARC1; p=none\nsecond",
      },
      ZONE,
    )

    expect(recordSet).toStrictEqual({
      Name: "_dmarc.example.com.",
      Type: "TXT",
      TTL: 60,
      ResourceRecords: [{ Value: '"v=DMARC1; p=none"' }, { Value: '"second"' }],
    })
    expect(toFormValues(recordSet, ZONE)).toStrictEqual({
      name: "_dmarc",
      type: "TXT",
      ttl: 60,
      values: "v=DMARC1; p=none\nsecond",
    })
  })

  it("passes other types through unchanged", () => {
    expect(
      toRecordSet(
        { name: "", type: "MX", ttl: 300, values: "10 mx.example.com" },
        ZONE,
      ).ResourceRecords,
    ).toStrictEqual([{ Value: "10 mx.example.com" }])
  })
})
