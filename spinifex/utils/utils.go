package utils

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/pterm/pterm"
)

// GenerateResourceID generates a unique resource ID with the given prefix.
// Format: {prefix}-{17 hex chars} using crypto/rand.
func GenerateResourceID(prefix string) string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b)[:17]
}

func MarshalToXML(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	if err := xmlutil.BuildXML(payload, enc); err != nil {
		slog.Error("BuildXML failed", "err", err)
		return nil, err
	}

	if err := enc.Flush(); err != nil {
		slog.Error("Flush failed", "err", err)
		return nil, err
	}

	return buf.Bytes(), nil
}

// GenerateXMLPayload wraps payload with the requested locationName tag.
func GenerateXMLPayload(locationName string, payload any) any {
	t := reflect.StructOf([]reflect.StructField{
		{
			Name: "Value",
			Type: reflect.TypeOf(payload),
			Tag:  reflect.StructTag(`locationName:"` + locationName + `"`),
		},
	})

	v := reflect.New(t).Elem()
	v.Field(0).Set(reflect.ValueOf(payload))
	return v.Interface()
}

// GenerateIAMXMLPayload wraps IAM output in the <ActionResponse><ActionResult>...</ActionResult></ActionResponse> structure.
func GenerateIAMXMLPayload(action string, payload any) any {
	resultName := action + "Result"
	resultWrapper := reflect.StructOf([]reflect.StructField{
		{
			Name: "Result",
			Type: reflect.TypeOf(payload),
			Tag:  reflect.StructTag(`locationName:"` + resultName + `"`),
		},
	})
	resultV := reflect.New(resultWrapper).Elem()
	resultV.Field(0).Set(reflect.ValueOf(payload))

	responseName := action + "Response"
	responseWrapper := reflect.StructOf([]reflect.StructField{
		{
			Name: "Response",
			Type: resultWrapper,
			Tag:  reflect.StructTag(`locationName:"` + responseName + `"`),
		},
	})
	responseV := reflect.New(responseWrapper).Elem()
	responseV.Field(0).Set(resultV)

	return responseV.Interface()
}

// GenerateErrorPayload serializes an ec2.ResponseError with the given code as JSON.
func GenerateErrorPayload(code string) (jsonResponse []byte) {
	var responseError ec2.ResponseError
	responseError.Code = aws.String(code)
	jsonResponse, err := json.Marshal(responseError)
	if err != nil {
		slog.Error("GenerateErrorPayload could not marshal JSON payload", "err", err)
		return nil
	}

	return jsonResponse
}

// ValidateErrorPayload decodes payload as an ec2.ResponseError and returns an error when a non-nil Code is detected.
func ValidateErrorPayload(payload []byte) (responseError ec2.ResponseError, err error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&responseError)

	if err == nil && responseError.Code != nil {
		return responseError, errors.New("ResponseError detected")
	}
	return responseError, nil
}

// UnmarshalJsonPayload decodes jsonData into input (already a pointer) using strict field checking.
func UnmarshalJsonPayload(input any, jsonData []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(input)
	if err != nil {
		return GenerateErrorPayload(awserrors.ErrorValidationError)
	}

	return nil
}

// ValidateKeyPairName validates that a key pair name contains only [A-Za-z0-9._-].
// Rejects empty names and returns ErrorInvalidKeyPairFormat on any invalid character.
func ValidateKeyPairName(name string) error {
	if name == "" {
		return errors.New("key name cannot be empty")
	}

	for _, char := range name {
		valid := (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '-' ||
			char == '_' ||
			char == '.'

		if !valid {
			return errors.New(awserrors.ErrorInvalidKeyPairFormat)
		}
	}

	return nil
}

func DownloadFileWithProgress(url string, name string, filename string, timeout time.Duration) (err error) {
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	intCh := make(chan os.Signal, 1)
	signal.Notify(intCh, os.Interrupt)
	go func() {
		<-intCh
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("request error: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("file create error: %v", err)
	}
	defer f.Close()

	cl := resp.ContentLength

	if cl > 0 {
		bar, _ := pterm.DefaultProgressbar.
			WithTitle(fmt.Sprintf("Downloading %s", name)).
			WithTotal(int(cl)).
			Start()

		reader := io.TeeReader(resp.Body, progressWriter(func(n int) {
			bar.Add(n)
		}))

		_, err = io.Copy(f, reader)
		_, _ = bar.Stop()
		if err != nil {
			return fmt.Errorf("copy error: %v", err)
		}
		return err
	} else {
		spin, _ := pterm.DefaultSpinner.
			WithText("Downloading (size unknown)...").
			Start()
		var written int64
		reader := io.TeeReader(resp.Body, progressWriter(func(n int) {
			written += int64(n)
			spin.UpdateText(fmt.Sprintf("Downloading %s (%s) ...", name, HumanBytes(SafeInt64ToUint64(written))))
		}))
		_, err = io.Copy(f, reader)
		_ = spin.Stop()

		if err != nil {
			return fmt.Errorf("copy error: %v", err)
		}
	}

	return nil
}

// progressWriter turns byte counts into a callback for UI updates.
type progressWriter func(n int)

func (pw progressWriter) Write(p []byte) (int, error) {
	pw(len(p))
	return len(p), nil
}

// HumanBytes formats a byte count using IEC binary suffixes (KiB, MiB, ...).
// Values below 1024 render as exact bytes.
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPEZY"[exp])
}

// HashMAC returns a deterministic locally-administered unicast MAC for id (SHA-256; first octet 0x02).
// id must be globally unique; callers sharing a base id across resource classes must compose a class tag (e.g. "dev:"+id).
func HashMAC(id string) string {
	sum := sha256.Sum256([]byte(id))
	b := make([]byte, 6)
	b[0] = 0x02
	copy(b[1:], sum[:5])
	return net.HardwareAddr(b).String()
}

// ClientIP returns the IP from a RemoteAddr, stripping the port. Handles both
// IPv4 and IPv6, and tolerates a RemoteAddr that is already a bare IP (no port).
func ClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return false
	}
	return info.IsDir()
}
