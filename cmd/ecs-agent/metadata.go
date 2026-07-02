package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// instanceMetadata is the IMDS-derived identity of the host the agent runs on.
type instanceMetadata struct {
	AccountID  string
	InstanceID string
	AZ         string
}

// fetchInstanceMetadata reads the instance-id, AZ and account-id from IMDSv2.
// base is the IMDS root, e.g. "http://169.254.169.254/latest".
func fetchInstanceMetadata(client *http.Client, base string) (instanceMetadata, error) {
	token, err := imdsToken(client, base)
	if err != nil {
		return instanceMetadata{}, err
	}

	instanceID, err := imdsGet(client, base+"/meta-data/instance-id", token)
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("instance-id: %w", err)
	}
	az, err := imdsGet(client, base+"/meta-data/placement/availability-zone", token)
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("availability-zone: %w", err)
	}

	doc, err := imdsGet(client, base+"/dynamic/instance-identity/document", token)
	if err != nil {
		return instanceMetadata{}, fmt.Errorf("identity document: %w", err)
	}
	var ident struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal([]byte(doc), &ident); err != nil {
		return instanceMetadata{}, fmt.Errorf("parse identity document: %w", err)
	}

	return instanceMetadata{
		AccountID:  ident.AccountID,
		InstanceID: strings.TrimSpace(instanceID),
		AZ:         strings.TrimSpace(az),
	}, nil
}

// imdsToken fetches an IMDSv2 session token.
func imdsToken(client *http.Client, base string) (string, error) {
	req, err := http.NewRequest(http.MethodPut, base+"/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("imds token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds token: status %d", resp.StatusCode)
	}
	return string(body), nil
}

// imdsGet performs a token-authenticated IMDS GET.
func imdsGet(client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-Aws-Ec2-Metadata-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(body), nil
}
