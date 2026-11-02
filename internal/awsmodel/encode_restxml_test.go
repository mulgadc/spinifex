package awsmodel_test

import (
	"cmp"
	"encoding/xml"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

const sdkEndpoint = "https://aws.example"

var sdkSession = session.Must(session.NewSession(&aws.Config{
	Region:           aws.String("us-east-1"),
	Endpoint:         aws.String(sdkEndpoint),
	Credentials:      credentials.AnonymousCredentials,
	S3ForcePathStyle: aws.Bool(true),
}))

// TestEncodeRequestRestXMLMatchesSDK checks the encoder against aws-sdk-go's
// restxml marshaller for the same input, so the expected wire form is the
// SDK's rather than one written by hand.
func TestEncodeRequestRestXMLMatchesSDK(t *testing.T) {
	r53 := route53.New(sdkSession)
	tests := []struct {
		name      string
		service   awsmodel.Service
		operation string
		input     map[string]any
		sdk       *request.Request
	}{
		{
			name:      "CreateHostedZone",
			service:   awsmodel.Route53,
			operation: "CreateHostedZone",
			input: map[string]any{
				"Name":             "example.com",
				"CallerReference":  "ref-1",
				"HostedZoneConfig": map[string]any{"Comment": "a & b", "PrivateZone": true},
				"VPC":              map[string]any{"VPCRegion": "us-east-1", "VPCId": "vpc-1"},
			},
			sdk: requestOf(r53.CreateHostedZoneRequest(&route53.CreateHostedZoneInput{
				Name:             aws.String("example.com"),
				CallerReference:  aws.String("ref-1"),
				HostedZoneConfig: &route53.HostedZoneConfig{Comment: aws.String("a & b"), PrivateZone: aws.Bool(true)},
				VPC:              &route53.VPC{VPCRegion: aws.String("us-east-1"), VPCId: aws.String("vpc-1")},
			})),
		},
		{
			name:      "ChangeResourceRecordSets",
			service:   awsmodel.Route53,
			operation: "ChangeResourceRecordSets",
			input: map[string]any{
				"HostedZoneId": "Z0123456789",
				"ChangeBatch": map[string]any{
					"Comment": "two changes",
					"Changes": []any{
						map[string]any{"Action": "UPSERT", "ResourceRecordSet": map[string]any{
							"Name": "www.example.com", "Type": "A", "TTL": int64(300),
							"ResourceRecords": []any{map[string]any{"Value": "192.0.2.1"}, map[string]any{"Value": "192.0.2.2"}},
						}},
						map[string]any{"Action": "DELETE", "ResourceRecordSet": map[string]any{
							"Name": "old.example.com", "Type": "TXT", "TTL": int64(60),
							"ResourceRecords": []any{map[string]any{"Value": `"v=1"`}},
						}},
					},
				},
			},
			sdk: requestOf(r53.ChangeResourceRecordSetsRequest(&route53.ChangeResourceRecordSetsInput{
				HostedZoneId: aws.String("Z0123456789"),
				ChangeBatch: &route53.ChangeBatch{
					Comment: aws.String("two changes"),
					Changes: []*route53.Change{
						{Action: aws.String("UPSERT"), ResourceRecordSet: &route53.ResourceRecordSet{
							Name: aws.String("www.example.com"), Type: aws.String("A"), TTL: aws.Int64(300),
							ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("192.0.2.1")}, {Value: aws.String("192.0.2.2")}},
						}},
						{Action: aws.String("DELETE"), ResourceRecordSet: &route53.ResourceRecordSet{
							Name: aws.String("old.example.com"), Type: aws.String("TXT"), TTL: aws.Int64(60),
							ResourceRecords: []*route53.ResourceRecord{{Value: aws.String(`"v=1"`)}},
						}},
					},
				},
			})),
		},
		{
			name:      "ChangeTagsForResource",
			service:   awsmodel.Route53,
			operation: "ChangeTagsForResource",
			input: map[string]any{
				"ResourceType":  "hostedzone",
				"ResourceId":    "Z0123456789",
				"AddTags":       []any{map[string]any{"Key": "team", "Value": "dns"}, map[string]any{"Key": "env", "Value": "prod"}},
				"RemoveTagKeys": []any{"old", "stale"},
			},
			sdk: requestOf(r53.ChangeTagsForResourceRequest(&route53.ChangeTagsForResourceInput{
				ResourceType:  aws.String("hostedzone"),
				ResourceId:    aws.String("Z0123456789"),
				AddTags:       []*route53.Tag{{Key: aws.String("team"), Value: aws.String("dns")}, {Key: aws.String("env"), Value: aws.String("prod")}},
				RemoveTagKeys: []*string{aws.String("old"), aws.String("stale")},
			})),
		},
		{
			name:      "UpdateHostedZoneComment without a comment",
			service:   awsmodel.Route53,
			operation: "UpdateHostedZoneComment",
			input:     map[string]any{"Id": "Z0123456789"},
			sdk:       requestOf(r53.UpdateHostedZoneCommentRequest(&route53.UpdateHostedZoneCommentInput{Id: aws.String("Z0123456789")})),
		},
		{
			name:      "ListResourceRecordSets",
			service:   awsmodel.Route53,
			operation: "ListResourceRecordSets",
			input:     map[string]any{"HostedZoneId": "Z0123456789", "StartRecordName": "www.example.com", "StartRecordType": "A", "MaxItems": int64(5)},
			sdk: requestOf(r53.ListResourceRecordSetsRequest(&route53.ListResourceRecordSetsInput{
				HostedZoneId: aws.String("Z0123456789"), StartRecordName: aws.String("www.example.com"), StartRecordType: aws.String("A"), MaxItems: aws.String("5"),
			})),
		},
		{
			name:      "DeleteHostedZone",
			service:   awsmodel.Route53,
			operation: "DeleteHostedZone",
			input:     map[string]any{"Id": "Z0123456789"},
			sdk:       requestOf(r53.DeleteHostedZoneRequest(&route53.DeleteHostedZoneInput{Id: aws.String("Z0123456789")})),
		},
		{
			name:      "S3 structure payload",
			service:   awsmodel.S3,
			operation: "PutBucketTagging",
			input: map[string]any{
				"Bucket":  "bucket",
				"Tagging": map[string]any{"TagSet": []any{map[string]any{"Key": "team", "Value": "storage"}}},
			},
			sdk: requestOf(s3.New(sdkSession).PutBucketTaggingRequest(&s3.PutBucketTaggingInput{
				Bucket:  aws.String("bucket"),
				Tagging: &s3.Tagging{TagSet: []*s3.Tag{{Key: aws.String("team"), Value: aws.String("storage")}}},
			})),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, test.sdk.Build())
			want := test.sdk.HTTPRequest
			wantBody, err := io.ReadAll(test.sdk.GetBody())
			require.NoError(t, err)

			got, err := awsmodel.EncodeRequest(test.service, test.operation, test.input)
			require.NoError(t, err)
			require.Equal(t, want.Method, got.Method)
			gotURL, err := url.Parse(got.URL(sdkEndpoint))
			require.NoError(t, err)
			// The SDK's api-2.json ends ChangeResourceRecordSets' path in a
			// slash that the Smithy model drops; the gateway accepts both.
			require.Equal(t, strings.TrimSuffix(want.URL.Path, "/"), gotURL.Path)
			require.Equal(t, want.URL.Query(), gotURL.Query())
			if len(wantBody) == 0 {
				require.Empty(t, got.Body)
				require.Empty(t, got.Header.Get("Content-Type"))
				return
			}
			require.Equal(t, canonicalXML(t, wantBody), canonicalXML(t, got.Body))
			require.Equal(t, "application/xml", got.Header.Get("Content-Type"))
		})
	}
}

func requestOf[O any](r *request.Request, _ O) *request.Request { return r }

// xmlTree is a parsed XML element. The SDK emits sibling elements in map
// order, so canonicalXML sorts them by name, keeping list items in order.
type xmlTree struct {
	XMLName  xml.Name   `xml:""`
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlTree  `xml:",any"`
}

func canonicalXML(t *testing.T, body []byte) xmlTree {
	t.Helper()
	var tree xmlTree
	require.NoError(t, xml.Unmarshal(body, &tree), string(body))
	var canonicalise func(*xmlTree)
	canonicalise = func(node *xmlTree) {
		node.Text = strings.TrimSpace(node.Text)
		slices.SortFunc(node.Attrs, func(a, b xml.Attr) int { return cmp.Compare(a.Name.Space+a.Name.Local, b.Name.Space+b.Name.Local) })
		slices.SortStableFunc(node.Children, func(a, b xmlTree) int { return cmp.Compare(a.XMLName.Local, b.XMLName.Local) })
		for i := range node.Children {
			canonicalise(&node.Children[i])
		}
	}
	canonicalise(&tree)
	return tree
}

func TestEncodeRequestRestXMLPutsRootInServiceNamespace(t *testing.T) {
	request, err := awsmodel.EncodeRequest(awsmodel.Route53, "CreateHostedZone", map[string]any{"Name": "example.com", "CallerReference": "ref"})
	require.NoError(t, err)
	tree := canonicalXML(t, request.Body)
	require.Equal(t, xml.Name{Space: "https://route53.amazonaws.com/doc/2013-04-01/", Local: "CreateHostedZoneRequest"}, tree.XMLName)
	require.Equal(t, "/2013-04-01/hostedzone", request.Path)
}
