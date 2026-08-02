package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/mulgadc/spinifex/internal/imdscreds"
)

// instanceMetadata is the IMDS-derived identity of the host the agent runs on.
type instanceMetadata struct {
	AccountID  string
	InstanceID string
	AZ         string
}

// fetchInstanceMetadata reads the instance-id, AZ and account-id from IMDSv2
// via the SDK IMDS client. base is the IMDS root, e.g.
// "http://169.254.169.254/latest".
func fetchInstanceMetadata(client *http.Client, base string) (instanceMetadata, error) {
	imdsClient := imdscreds.NewClient(client, base)
	ctx := context.Background()

	instanceID, err := getMetadata(ctx, imdsClient, "instance-id")
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("instance-id: %w", err)
	}
	az, err := getMetadata(ctx, imdsClient, "placement/availability-zone")
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("availability-zone: %w", err)
	}

	doc, err := imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("identity document: %w", err)
	}

	return instanceMetadata{
		AccountID:  doc.AccountID,
		InstanceID: strings.TrimSpace(instanceID),
		AZ:         strings.TrimSpace(az),
	}, nil
}

// getMetadata reads a single meta-data path and returns its body as a string.
func getMetadata(ctx context.Context, imdsClient *imds.Client, path string) (string, error) {
	out, err := imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{Path: path})
	if err != nil {
		return "", err
	}
	defer out.Content.Close()
	body, err := io.ReadAll(out.Content)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
