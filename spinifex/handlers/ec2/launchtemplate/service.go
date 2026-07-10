package handlers_ec2_launchtemplate

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// LaunchTemplateService defines the interface for EC2 launch template operations.
type LaunchTemplateService interface {
	CreateLaunchTemplate(ctx context.Context, input *ec2.CreateLaunchTemplateInput, accountID string) (*ec2.CreateLaunchTemplateOutput, error)
	CreateLaunchTemplateVersion(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, accountID string) (*ec2.CreateLaunchTemplateVersionOutput, error)
	DeleteLaunchTemplate(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, accountID string) (*ec2.DeleteLaunchTemplateOutput, error)
	DeleteLaunchTemplateVersions(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, accountID string) (*ec2.DeleteLaunchTemplateVersionsOutput, error)
	ModifyLaunchTemplate(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, accountID string) (*ec2.ModifyLaunchTemplateOutput, error)
	DescribeLaunchTemplates(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, accountID string) (*ec2.DescribeLaunchTemplatesOutput, error)
	DescribeLaunchTemplateVersions(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, accountID string) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
}

// LaunchTemplateHeader is the only mutable record for a template: the default
// version pointer plus tags. It is the source of truth for the template's
// existence and is keyed by id (account.lt-<id>).
type LaunchTemplateHeader struct {
	LaunchTemplateId     string            `json:"launch_template_id"` // "lt-..."
	LaunchTemplateName   string            `json:"launch_template_name"`
	AccountID            string            `json:"account_id"`
	CreatedBy            string            `json:"created_by"`
	CreateTime           time.Time         `json:"create_time"`
	DefaultVersionNumber int64             `json:"default_version_number"`
	Tags                 map[string]string `json:"tags,omitempty"`
}

// LaunchTemplateVersionRec is a single immutable version body, keyed by id and
// number (account.lt-<id>.v<n>). Once written it is never mutated.
type LaunchTemplateVersionRec struct {
	VersionNumber      int64                           `json:"version_number"`
	VersionDescription string                          `json:"version_description,omitempty"`
	CreateTime         time.Time                       `json:"create_time"`
	CreatedBy          string                          `json:"created_by"`
	Data               *ec2.ResponseLaunchTemplateData `json:"data"`
}
