package instance

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// AWS's page size when a Describe call names none.
const defaultMaxRecords = 100

// Joins the fields of a compound page key. It sorts below every printable byte,
// so a key compares field by field exactly as the fields do.
const pageKeySeparator = "\x00"

// Page returns one page of items, sorted by key. The Marker is base64 of the next item's key, as AWS
// issues it; a resume starts at the first key not below it, so that item's deletion skips nothing.
// MaxRecords outside 1-100 is 100.
func Page[T any](items []T, key func(T) string, maxRecords *int64, marker *string) ([]T, *string, error) {
	req := paging.Request{Limit: defaultMaxRecords}
	if encoded := aws.StringValue(marker); encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Marker %q was not issued by this service", encoded)
		}
		req.From = string(decoded)
	}
	if requested := aws.Int64Value(maxRecords); requested > 0 && requested < defaultMaxRecords {
		req.Limit = int(requested)
	}
	page, token := paging.Page(items, key, req)
	if token == "" {
		return page, nil, nil
	}
	next, err := paging.DecodeToken(token)
	if err != nil {
		return nil, nil, fmt.Errorf("re-encode page marker: %w", err)
	}
	return page, aws.String(base64.StdEncoding.EncodeToString([]byte(next))), nil
}

func pageKey(fields ...string) string {
	return strings.Join(fields, pageKeySeparator)
}

// The key each paged listing is already sorted by.
func parameterPageKey(p *rds.Parameter) string { return aws.StringValue(p.ParameterName) }

func dbInstancePageKey(i *rds.DBInstance) string { return aws.StringValue(i.DBInstanceIdentifier) }

func dbSnapshotPageKey(s *rds.DBSnapshot) string { return aws.StringValue(s.DBSnapshotIdentifier) }

func automatedBackupPageKey(b *rds.DBInstanceAutomatedBackup) string {
	return aws.StringValue(b.DBInstanceIdentifier)
}

func subnetGroupPageKey(g *rds.DBSubnetGroup) string { return aws.StringValue(g.DBSubnetGroupName) }

func parameterGroupPageKey(g *rds.DBParameterGroup) string {
	return aws.StringValue(g.DBParameterGroupName)
}

// EngineVersionPageKey returns the engine-plus-version key a DescribeDBEngineVersions listing is
// sorted and paged by.
func EngineVersionPageKey(v *rds.DBEngineVersion) string {
	return pageKey(aws.StringValue(v.Engine), aws.StringValue(v.EngineVersion))
}

// OrderableOptionPageKey returns the engine, version and instance class key a
// DescribeOrderableDBInstanceOptions listing is sorted and paged by.
func OrderableOptionPageKey(o *rds.OrderableDBInstanceOption) string {
	return pageKey(aws.StringValue(o.Engine), aws.StringValue(o.EngineVersion), aws.StringValue(o.DBInstanceClass))
}
