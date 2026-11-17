package image

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

// A malformed ID is refused before any NATS round-trip, so the nil connection
// is never used.
func TestImageDeprecation_MalformedImageID(t *testing.T) {
	for _, id := range []string{"ami-zzz", "img-0123456789abcdef0", "ami-0123456789abcdef"} {
		_, err := EnableImageDeprecation(context.Background(), &ec2.EnableImageDeprecationInput{
			ImageId: aws.String(id), DeprecateAt: aws.Time(time.Now().AddDate(1, 0, 0)),
		}, nil, "123456789012")
		code, msg, _ := awserrors.ResolveErrorDetail(err)
		assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, code, id)
		assert.Equal(t, "The image ID '"+id+"' is malformed", msg, id)

		_, err = DisableImageDeprecation(context.Background(), &ec2.DisableImageDeprecationInput{ImageId: aws.String(id)}, nil, "123456789012")
		code, msg, _ = awserrors.ResolveErrorDetail(err)
		assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, code, id)
		assert.Equal(t, "The image ID '"+id+"' is malformed", msg, id)
	}
}
