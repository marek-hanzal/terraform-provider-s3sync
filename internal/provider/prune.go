package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/objects"
)

type pruneResource struct{ client *objects.Client }
type pruneModel struct {
	ID      types.String `tfsdk:"id"`
	Bucket  types.String `tfsdk:"bucket"`
	Prefix  types.String `tfsdk:"prefix"`
	Keep    types.Set    `tfsdk:"keep"`
	Pending types.Set    `tfsdk:"pending"`
}

func (r *pruneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_prune"
}
func (r *pruneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Deletes every non-kept key in an explicitly exclusive prefix during apply. Sequence after publication/cache invalidation. Destroy only forgets this cleanup gate.", Attributes: map[string]schema.Attribute{
		"id":      schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"bucket":  schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		"prefix":  schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Description: "Exclusive nonempty relative prefix ending in '/'. This resource owns deletion of all objects inside it."},
		"keep":    schema.SetAttribute{Required: true, ElementType: types.StringType, Description: "Relative object keys to retain inside this prefix. An empty set explicitly removes every object inside the prefix."},
		"pending": schema.SetAttribute{Computed: true, ElementType: types.StringType, Description: "Observed obsolete keys; the planned value is empty after successful cleanup."},
	}}
}
func (r *pruneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureClient(req.ProviderData, &resp.Diagnostics)
}
func (r *pruneResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model pruneModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !model.Bucket.IsUnknown() && !model.Prefix.IsUnknown() {
		if err := objects.ValidateDestination(model.Bucket.ValueString(), model.Prefix.ValueString(), true); err != nil {
			resp.Diagnostics.AddError("Invalid cleanup destination", err.Error())
		}
	}
	if !model.Keep.IsUnknown() && !model.Keep.IsNull() {
		for _, value := range model.Keep.Elements() {
			if value.IsUnknown() {
				return
			}
		}
		var keep []string
		resp.Diagnostics.Append(model.Keep.ElementsAs(ctx, &keep, false)...)
		if err := objects.ValidateKeys(keep); err != nil {
			resp.Diagnostics.AddError("Invalid retained key", err.Error())
		}
	}
}
func (r *pruneResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("pending"), types.SetValueMust(types.StringType, nil))...)
}
func (r *pruneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var model pruneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.client == nil {
		resp.Diagnostics.AddError("Unknown S3 configuration", "S3 credentials and endpoint must be known before refresh.")
		return
	}
	var keep []string
	resp.Diagnostics.Append(model.Keep.ElementsAs(ctx, &keep, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	remote, err := r.client.List(ctx, model.Bucket.ValueString(), model.Prefix.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("S3 cleanup refresh failed", err.Error())
		return
	}
	for _, key := range keep {
		delete(remote, key)
	}
	pending := make([]string, 0, len(remote))
	for key := range remote {
		pending = append(pending, key)
	}
	sort.Strings(pending)
	model.Pending, _ = types.SetValueFrom(ctx, types.StringType, pending)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (r *pruneResource) prune(ctx context.Context, model *pruneModel, diagnostics *diag.Diagnostics) {
	if r.client == nil {
		diagnostics.AddError("Unknown S3 configuration", "S3 credentials and endpoint must be known before cleanup.")
		return
	}
	var keep []string
	diagnostics.Append(model.Keep.ElementsAs(ctx, &keep, false)...)
	if diagnostics.HasError() {
		return
	}
	if _, err := r.client.Prune(ctx, model.Bucket.ValueString(), model.Prefix.ValueString(), keep); err != nil {
		diagnostics.AddError("S3 cleanup failed", err.Error())
		return
	}
	model.ID = types.StringValue(resourceID(model.Bucket.ValueString(), model.Prefix.ValueString()))
	model.Pending = types.SetValueMust(types.StringType, nil)
}
func (r *pruneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model pruneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.prune(ctx, &model, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (r *pruneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var model pruneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.prune(ctx, &model, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (r *pruneResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}
