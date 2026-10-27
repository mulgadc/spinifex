package awsapi

import (
	"bytes"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
)

// XMLNamespace is the namespace every Route 53 request and response carries.
const XMLNamespace = "https://route53.amazonaws.com/doc/2013-04-01/"

// bindInput fills an SDK input struct the way the REST-XML protocol lays it
// out: members tagged location:"uri" from the path, location:"querystring"
// from the query, and the rest from the XML body.
func bindInput(input any, path map[string]string, query url.Values, body []byte) error {
	v := reflect.ValueOf(input).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		var raw string
		var ok bool
		switch f.Tag.Get("location") {
		case "uri":
			raw, ok = path[f.Tag.Get("locationName")]
		case "querystring":
			raw, ok = query.Get(f.Tag.Get("locationName")), query.Has(f.Tag.Get("locationName"))
		default:
			continue
		}
		if !ok || f.Type != reflect.TypeFor[*string]() {
			continue
		}
		v.Field(i).Set(reflect.ValueOf(aws.String(raw)))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := xmlutil.UnmarshalXML(input, xml.NewDecoder(bytes.NewReader(body)), ""); err != nil {
		return awserrors.Errorf(awserrors.ErrorInvalidInput, "the request body is not valid XML: %v", err)
	}
	return nil
}

// writeXML renders output under a <ActionResponse> root in the Route 53
// namespace, plus the Location header CreateHostedZone carries.
func writeXML(w http.ResponseWriter, r *http.Request, action string, status int, output any) error {
	if loc := locationHeader(output); loc != "" {
		w.Header().Set("Location", "https://"+r.Host+loc)
	}
	body, err := awsxml.Marshal(rootElement(action+"Response", output))
	if err != nil {
		slog.ErrorContext(r.Context(), "Route 53: marshal response", "action", action, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
	return nil
}

func rootElement(name string, payload any) any {
	t := reflect.StructOf([]reflect.StructField{{
		Name: "Value",
		Type: reflect.TypeOf(payload),
		Tag:  reflect.StructTag(`locationName:"` + name + `" xmlURI:"` + XMLNamespace + `"`),
	}})
	v := reflect.New(t).Elem()
	v.Field(0).Set(reflect.ValueOf(payload))
	return v.Interface()
}

func locationHeader(output any) string {
	v := reflect.ValueOf(output)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ""
	}
	f := v.Elem().FieldByName("Location")
	if !f.IsValid() || f.Kind() != reflect.Pointer || f.IsNil() || f.Elem().Kind() != reflect.String {
		return ""
	}
	return f.Elem().String()
}

func patternNames(pattern string) []string {
	var names []string
	for seg := range strings.SplitSeq(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			names = append(names, seg[1:len(seg)-1])
		}
	}
	return names
}
