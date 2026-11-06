package lambdaenv_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/lambdaenv"
)

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want error
	}{
		"absent": {},
		"literal values and configurable settings": {vars: map[string]string{"GREETING": "hello $WORLD", "PATH": "/my/bin", "AWS_ENDPOINT_URL": "https://example.test"}},
		"maximum size":                   {vars: map[string]string{"AA": strings.Repeat("x", 4094)}},
		"empty name":                     {vars: map[string]string{"": "x"}, want: lambdaenv.ErrName},
		"one-character name":             {vars: map[string]string{"A": "x"}, want: lambdaenv.ErrName},
		"invalid character":              {vars: map[string]string{"bad-name": "x"}, want: lambdaenv.ErrName},
		"reserved credential":            {vars: map[string]string{"AWS_SECRET_ACCESS_KEY": "x"}, want: lambdaenv.ErrReserved},
		"invalid name precedes reserved": {vars: map[string]string{"A": "x", "AWS_REGION": "x"}, want: lambdaenv.ErrName},
		"nul value":                      {vars: map[string]string{"VALUE": "x\x00"}, want: lambdaenv.ErrValue},
		"over maximum":                   {vars: map[string]string{"AA": strings.Repeat("x", 4095)}, want: lambdaenv.ErrSize},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lambdaenv.Validate(tc.vars); !errors.Is(got, tc.want) {
				t.Fatalf("Validate() = %v, want %v", got, tc.want)
			}
		})
	}
}
