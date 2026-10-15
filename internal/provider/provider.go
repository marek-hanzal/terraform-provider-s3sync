package provider

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/objects"
)

type syncProvider struct{ version string }

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &syncProvider{version: version} }
}

func (p *syncProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName, resp.Version = "s3sync", p.version
}

func (p *syncProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Bulk synchronization to S3-compatible object storage. No storage provisioning or consumer-specific publication policy.", Attributes: map[string]schema.Attribute{
		"endpoint":                schema.StringAttribute{Optional: true, Description: "Custom S3 endpoint. Path-style addressing is used. Omit for AWS S3."},
		"region":                  schema.StringAttribute{Optional: true, Description: "Signing region; defaults to us-east-1."},
		"access_key":              schema.StringAttribute{Optional: true, Sensitive: true, Description: "Explicit access key, or S3SYNC_ACCESS_KEY. No ambient AWS credential chain."},
		"secret_key":              schema.StringAttribute{Optional: true, Sensitive: true, Description: "Explicit secret key, or S3SYNC_SECRET_KEY."},
		"etag_hash":               schema.StringAttribute{Optional: true, Description: "opaque (default), md5, or sha256. Explicit hash modes permit adopting matching single-part objects; use only when the storage guarantees that ETag algorithm."},
		"max_attempts":            schema.Int64Attribute{Optional: true, Description: "AWS SDK maximum attempts per request, including the initial attempt; defaults to 5."},
		"request_timeout_seconds": schema.Int64Attribute{Optional: true, Description: "HTTP request timeout, defaults to 120 seconds."},
	}}
}

func (p *syncProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model struct {
		Endpoint    types.String `tfsdk:"endpoint"`
		Region      types.String `tfsdk:"region"`
		AccessKey   types.String `tfsdk:"access_key"`
		SecretKey   types.String `tfsdk:"secret_key"`
		ETagHash    types.String `tfsdk:"etag_hash"`
		MaxAttempts types.Int64  `tfsdk:"max_attempts"`
		Timeout     types.Int64  `tfsdk:"request_timeout_seconds"`
	}
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if model.Endpoint.IsUnknown() || model.Region.IsUnknown() || model.AccessKey.IsUnknown() || model.SecretKey.IsUnknown() || model.ETagHash.IsUnknown() || model.MaxAttempts.IsUnknown() || model.Timeout.IsUnknown() {
		return // Terraform may create the storage credentials earlier in this apply.
	}
	config := objects.Config{Endpoint: model.Endpoint.ValueString(), Region: model.Region.ValueString(), AccessKey: model.AccessKey.ValueString(), SecretKey: model.SecretKey.ValueString(), ETagHash: model.ETagHash.ValueString(), MaxAttempts: 5, RequestTimeout: 120 * time.Second}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.ETagHash == "" {
		config.ETagHash = "opaque"
	}
	if model.AccessKey.IsNull() {
		config.AccessKey = os.Getenv("S3SYNC_ACCESS_KEY")
	}
	if model.SecretKey.IsNull() {
		config.SecretKey = os.Getenv("S3SYNC_SECRET_KEY")
	}
	if !model.MaxAttempts.IsNull() {
		if model.MaxAttempts.ValueInt64() < 1 || model.MaxAttempts.ValueInt64() > 20 {
			resp.Diagnostics.AddError("Invalid retry limit", "max_attempts must be between 1 and 20.")
			return
		}
		config.MaxAttempts = int(model.MaxAttempts.ValueInt64())
	}
	if !model.Timeout.IsNull() {
		if model.Timeout.ValueInt64() < 1 || model.Timeout.ValueInt64() > 3600 {
			resp.Diagnostics.AddError("Invalid request timeout", "request_timeout_seconds must be between 1 and 3600.")
			return
		}
		config.RequestTimeout = time.Duration(model.Timeout.ValueInt64()) * time.Second
	}
	client, err := objects.New(ctx, config)
	if err != nil {
		resp.Diagnostics.AddError("Invalid S3 configuration", err.Error())
		return
	}
	resp.ResourceData = client
}

func (p *syncProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{func() resource.Resource { return &filesResource{} }, func() resource.Resource { return &pruneResource{} }}
}
func (p *syncProvider) DataSources(_ context.Context) []func() datasource.DataSource { return nil }
