package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/objects"
)

type filesResource struct{ client *objects.Client }
type filesModel struct {
	ID              types.String `tfsdk:"id"`
	Bucket          types.String `tfsdk:"bucket"`
	Prefix          types.String `tfsdk:"prefix"`
	Files           types.Map    `tfsdk:"files"`
	Concurrency     types.Int64  `tfsdk:"concurrency"`
	DeleteOnDestroy types.Bool   `tfsdk:"delete_on_destroy"`
	Inventory       types.Map    `tfsdk:"inventory"`
	AppliedDigest   types.String `tfsdk:"applied_digest"`
}
type fileModel struct {
	Source      types.String `tfsdk:"source"`
	SHA256      types.String `tfsdk:"sha256"`
	ContentType types.String `tfsdk:"content_type"`
}
type recordModel struct {
	SHA256      types.String `tfsdk:"sha256"`
	ETag        types.String `tfsdk:"etag"`
	Size        types.Int64  `tfsdk:"size"`
	ContentType types.String `tfsdk:"content_type"`
}

var recordType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"sha256": types.StringType, "etag": types.StringType, "size": types.Int64Type, "content_type": types.StringType,
}}

func (r *filesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_files"
}
func (r *filesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Upserts one manifest of files using a bulk remote listing. Removed keys are retained; prune is a separate, explicitly ordered resource.", Attributes: map[string]schema.Attribute{
		"id":     schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"bucket": schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		"prefix": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Description: "Empty or a relative key prefix ending in '/'."},
		"files": schema.MapNestedAttribute{Required: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"source":       schema.StringAttribute{Required: true, Description: "Absolute path to a regular local file."},
			"sha256":       schema.StringAttribute{Required: true, Description: "Expected lowercase hexadecimal SHA-256; validated before any write."},
			"content_type": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("application/octet-stream")},
		}}},
		"concurrency":       schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(8)},
		"delete_on_destroy": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "Opt in to deleting only the keys acknowledged in this resource's inventory on destroy."},
		"inventory": schema.MapNestedAttribute{Computed: true, Description: "Acknowledged hashes and ETags, including retained keys. No media or credentials.", NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"sha256": schema.StringAttribute{Computed: true}, "etag": schema.StringAttribute{Computed: true}, "size": schema.Int64Attribute{Computed: true}, "content_type": schema.StringAttribute{Computed: true},
		}}},
		"applied_digest": schema.StringAttribute{Computed: true, Description: "Manifest digest verified by the last successful reconciliation. Refresh invalidates it on observed body drift."},
	}}
}

func configureClient(data any, diagnostics *diag.Diagnostics) *objects.Client {
	if data == nil {
		return nil
	}
	client, ok := data.(*objects.Client)
	if !ok {
		diagnostics.AddError("Invalid provider configuration", "Expected S3 client.")
		return nil
	}
	return client
}
func (r *filesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureClient(req.ProviderData, &resp.Diagnostics)
}

func readFiles(ctx context.Context, value types.Map) (map[string]objects.File, diag.Diagnostics, bool) {
	if value.IsNull() || value.IsUnknown() {
		return nil, nil, false
	}
	for _, value := range value.Elements() {
		object, ok := value.(types.Object)
		if !ok || object.IsUnknown() || object.IsNull() {
			return nil, nil, false
		}
		for _, value := range object.Attributes() {
			if value.IsUnknown() {
				return nil, nil, false
			}
		}
	}
	var models map[string]fileModel
	diagnostics := value.ElementsAs(ctx, &models, false)
	files := make(map[string]objects.File, len(models))
	for key, model := range models {
		files[key] = objects.File{Source: model.Source.ValueString(), SHA256: model.SHA256.ValueString(), ContentType: model.ContentType.ValueString()}
	}
	return files, diagnostics, true
}
func readInventory(ctx context.Context, value types.Map) (map[string]objects.Record, diag.Diagnostics) {
	result := make(map[string]objects.Record)
	if value.IsNull() || value.IsUnknown() {
		return result, nil
	}
	var models map[string]recordModel
	diagnostics := value.ElementsAs(ctx, &models, false)
	for key, model := range models {
		result[key] = objects.Record{SHA256: model.SHA256.ValueString(), ETag: model.ETag.ValueString(), Size: model.Size.ValueInt64(), ContentType: model.ContentType.ValueString()}
	}
	return result, diagnostics
}
func encodeInventory(ctx context.Context, inventory map[string]objects.Record) (types.Map, diag.Diagnostics) {
	models := make(map[string]recordModel, len(inventory))
	for key, record := range inventory {
		models[key] = recordModel{SHA256: types.StringValue(record.SHA256), ETag: types.StringValue(record.ETag), Size: types.Int64Value(record.Size), ContentType: types.StringValue(record.ContentType)}
	}
	return types.MapValueFrom(ctx, recordType, models)
}
func resourceID(bucket, prefix string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + prefix))
	return hex.EncodeToString(sum[:])
}

func (r *filesResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model filesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !model.Bucket.IsUnknown() && !model.Prefix.IsUnknown() {
		if err := objects.ValidateDestination(model.Bucket.ValueString(), model.Prefix.ValueString(), false); err != nil {
			resp.Diagnostics.AddError("Invalid destination", err.Error())
		}
	}
	files, diagnostics, known := readFiles(ctx, model.Files)
	resp.Diagnostics.Append(diagnostics...)
	if known {
		if err := objects.ValidateFiles(files); err != nil {
			resp.Diagnostics.AddError("Invalid file manifest", err.Error())
		}
	}
	if !model.Concurrency.IsNull() && !model.Concurrency.IsUnknown() && (model.Concurrency.ValueInt64() < 1 || model.Concurrency.ValueInt64() > 64) {
		resp.Diagnostics.AddError("Invalid concurrency", "concurrency must be between 1 and 64.")
	}
}

func (r *filesResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var model filesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	files, diagnostics, known := readFiles(ctx, model.Files)
	resp.Diagnostics.Append(diagnostics...)
	if !known || resp.Diagnostics.HasError() {
		return
	}
	digest := objects.Digest(files)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("applied_digest"), digest)...)
	if !req.State.Raw.IsNull() {
		var previous filesModel
		resp.Diagnostics.Append(req.State.Get(ctx, &previous)...)
		if previous.AppliedDigest.ValueString() != digest {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("inventory"), types.MapUnknown(recordType))...)
		}
	}
}

func (r *filesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var model filesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.client == nil {
		resp.Diagnostics.AddError("Unknown S3 configuration", "S3 credentials and endpoint must be known before refresh.")
		return
	}
	files, diagnostics, known := readFiles(ctx, model.Files)
	resp.Diagnostics.Append(diagnostics...)
	inventory, diagnostics := readInventory(ctx, model.Inventory)
	resp.Diagnostics.Append(diagnostics...)
	if !known || resp.Diagnostics.HasError() {
		return
	}
	remote, err := r.client.List(ctx, model.Bucket.ValueString(), model.Prefix.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("S3 refresh failed", err.Error())
		return
	}
	for key, file := range files {
		ack, exists := inventory[key]
		actual, present := remote[key]
		if !exists || !present || ack.SHA256 != file.SHA256 || ack.ETag == "" || actual.ETag != ack.ETag || actual.Size != ack.Size {
			model.AppliedDigest = types.StringValue("")
			break
		}
	}
	// Retired ownership disappears once an explicitly separate cleanup removed the body.
	for key := range inventory {
		_, desired := files[key]
		_, present := remote[key]
		if !desired && !present {
			delete(inventory, key)
		}
	}
	model.Inventory, diagnostics = encodeInventory(ctx, inventory)
	resp.Diagnostics.Append(diagnostics...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func (r *filesResource) reconcile(ctx context.Context, model *filesModel, diagnostics *diag.Diagnostics) {
	if r.client == nil {
		diagnostics.AddError("Unknown S3 configuration", "S3 credentials and endpoint must be known before apply.")
		return
	}
	files, decoded, known := readFiles(ctx, model.Files)
	diagnostics.Append(decoded...)
	previous, decoded := readInventory(ctx, model.Inventory)
	diagnostics.Append(decoded...)
	if !known || diagnostics.HasError() {
		diagnostics.AddError("Unknown file manifest", "Every file must have a known source and SHA-256 before apply.")
		return
	}
	result, err := r.client.Reconcile(ctx, objects.Request{Bucket: model.Bucket.ValueString(), Prefix: model.Prefix.ValueString(), Files: files, Previous: previous, Concurrency: int(model.Concurrency.ValueInt64())})
	model.Inventory, decoded = encodeInventory(ctx, result.Inventory)
	diagnostics.Append(decoded...)
	model.ID = types.StringValue(resourceID(model.Bucket.ValueString(), model.Prefix.ValueString()))
	if err != nil {
		model.AppliedDigest = types.StringValue("")
		diagnostics.AddError("S3 synchronization failed", err.Error())
		return
	}
	model.AppliedDigest = types.StringValue(objects.Digest(files))
}
func (r *filesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model filesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.reconcile(ctx, &model, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (r *filesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var model filesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The plan may deliberately mark computed inventory unknown; recover acknowledged ownership from prior state.
	var previous filesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &previous)...)
	if resp.Diagnostics.HasError() {
		return
	}
	model.Inventory = previous.Inventory
	r.reconcile(ctx, &model, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (r *filesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var model filesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() || !model.DeleteOnDestroy.ValueBool() {
		return
	}
	if r.client == nil {
		resp.Diagnostics.AddError("Unknown S3 configuration", "S3 credentials and endpoint must be known before destroy.")
		return
	}
	inventory, diagnostics := readInventory(ctx, model.Inventory)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.Remove(ctx, model.Bucket.ValueString(), model.Prefix.ValueString(), inventory); err != nil {
		resp.Diagnostics.AddError("S3 removal failed", err.Error())
	}
}
