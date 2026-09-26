package generic

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"

	"terraform_provider/netconf"
	"terraform_provider/patch"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// ConfigResource is the generic schema-driven resource.
type ConfigResource struct {
	client     netconf.Client
	host       string
	idx        map[string]*patch.NodeInfo
	nodes      []patch.SchemaNode
	tfSchema   schema.Schema
	tfType     tftypes.Object
	schemaJSON string
	loadErr    error
}

// NewConfigResourceFromSchema loads the provider's embedded schema (raw or
// gzipped JSON) and returns the resource. A schema that fails to load or
// validate is reported when Terraform asks for the resource's schema; without
// the resource, Terraform would only say the resource type is not supported.
func NewConfigResourceFromSchema(raw []byte) *ConfigResource {
	idx, nodes, err := LoadSchema(raw)
	if err == nil && len(nodes) > 0 {
		err = ValidateNames(nodes[0].Children, nodes[0].Name)
	}
	if err != nil {
		return &ConfigResource{loadErr: err}
	}
	return NewConfigResource(idx, nodes, string(raw))
}

// NewConfigResource creates a ConfigResource from pre-loaded schema data.
func NewConfigResource(idx map[string]*patch.NodeInfo, nodes []patch.SchemaNode, schemaJSON string) *ConfigResource {
	r := &ConfigResource{
		idx:        idx,
		nodes:      nodes,
		tfSchema:   BuildSchema(nodes),
		schemaJSON: schemaJSON,
	}
	if t, ok := r.tfSchema.Type().TerraformType(context.Background()).(tftypes.Object); ok {
		r.tfType = t
	} else {
		r.loadErr = fmt.Errorf("resource schema type is not an object")
	}
	return r
}

// ProviderData is the interface the generic resource expects from provider configuration.
type ProviderData interface {
	GetClient() netconf.Client
	GetHost() string
}

func (r *ConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(ProviderData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected generic.ProviderData, got %T", req.ProviderData))
		return
	}
	r.client = pd.GetClient()
	r.host = pd.GetHost()
}

func (r *ConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "terraform-provider-" + req.ProviderTypeName
}

func (r *ConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	if r.loadErr != nil {
		resp.Diagnostics.AddError("Invalid embedded schema", r.loadErr.Error())
		return
	}
	resp.Schema = r.tfSchema
}

// Create loads the planned configuration into the device (merge) and commits.
func (r *ConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	plan := req.Plan.Raw
	root, err := ValueToConfig(plan, r.configNodes())
	if err != nil {
		resp.Diagnostics.AddError("Failed to build configuration", err.Error())
		return
	}
	if err := r.load(root); err != nil {
		resp.Diagnostics.AddError("Failed while applying configuration", err.Error())
		return
	}
	if err := r.client.SendCommit(); err != nil {
		resp.Diagnostics.AddError("Failed while committing configuration", err.Error())
		return
	}
	state, err := r.readState(plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read configuration", err.Error())
		return
	}
	resp.State.Raw = state
}

// Read refreshes the state from the device's configuration.
func (r *ConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	state, err := r.readState(req.State.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read configuration", err.Error())
		return
	}
	resp.State.Raw = state
}

// Update applies the difference between the device's configuration and the
// plan as one patch and commits it. If the device still differs afterwards,
// it loads the whole planned configuration (merge) and commits again.
func (r *ConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	plan := req.Plan.Raw
	planRoot, err := ValueToConfig(plan, r.configNodes())
	if err != nil {
		resp.Diagnostics.AddError("Failed to build configuration", err.Error())
		return
	}
	planMap := patch.LeafMapWithSchema(planRoot, r.idx)

	device, err := r.deviceConfig()
	if err != nil {
		resp.Diagnostics.AddError("Failed while reading current configuration", err.Error())
		return
	}
	diff := patch.ComputeDiff(patch.LeafMapWithSchema(device, r.idx), planMap)

	if len(diff) > 0 {
		resourceName := r.resourceName(plan)
		patchXML, err := patch.CreateDiffPatch(diff, resourceName)
		if err != nil {
			resp.Diagnostics.AddError("Failed to build NETCONF patch", err.Error())
			return
		}
		debugPatchUpdate(resourceName, device, planRoot, diff, string(patchXML))
		if err := r.client.SendUpdate("", string(patchXML), false); err != nil {
			resp.Diagnostics.AddError("Failed while sending diff patch", err.Error())
			return
		}
		if err := r.client.SendCommit(); err != nil {
			resp.Diagnostics.AddError("Failed while committing configuration", err.Error())
			return
		}

		verified, err := r.deviceConfig()
		if err != nil {
			resp.Diagnostics.AddError("Failed while reading patched configuration", err.Error())
			return
		}
		if len(patch.ComputeDiff(patch.LeafMapWithSchema(verified, r.idx), planMap)) > 0 {
			if err := r.load(planRoot); err != nil {
				resp.Diagnostics.AddError("Patch had no effect and fallback update failed", err.Error())
				return
			}
			if err := r.client.SendCommit(); err != nil {
				resp.Diagnostics.AddError("Fallback update commit failed", err.Error())
				return
			}
		}
	}

	state, err := r.readState(plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read configuration", err.Error())
		return
	}
	resp.State.Raw = state
}

// Delete removes everything in the state from the device and commits.
func (r *ConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	stateRoot, err := ValueToConfig(req.State.Raw, r.configNodes())
	if err != nil {
		resp.Diagnostics.AddError("Failed to build configuration", err.Error())
		return
	}
	diff := patch.ComputeDiff(patch.LeafMapWithSchema(stateRoot, r.idx),
		patch.LeafMapWithSchema(&patch.Node{Tag: "configuration"}, r.idx))
	if len(diff) == 0 {
		return
	}
	patchXML, err := patch.CreateDiffPatch(diff, r.resourceName(req.State.Raw))
	if err != nil {
		resp.Diagnostics.AddError("Failed to build delete patch", err.Error())
		return
	}
	if err := r.client.SendUpdate("", string(patchXML), false); err != nil {
		resp.Diagnostics.AddError("Failed while deleting configuration", err.Error())
		return
	}
	if err := r.client.SendCommit(); err != nil {
		resp.Diagnostics.AddError("Failed while committing configuration", err.Error())
		return
	}
}

// configNodes returns the schema nodes under <configuration>.
func (r *ConfigResource) configNodes() []patch.SchemaNode {
	if len(r.nodes) == 0 {
		return nil
	}
	return r.nodes[0].Children
}

// rawConfiguration carries a <configuration> element's content as raw XML.
type rawConfiguration struct {
	XMLName xml.Name `xml:"configuration"`
	Inner   []byte   `xml:",innerxml"`
}

// deviceConfig reads the device's configuration, keeping only what the schema
// models, in schema order: the form the plan is compared with.
func (r *ConfigResource) deviceConfig() (*patch.Node, error) {
	v, err := r.deviceValue()
	if err != nil {
		return nil, err
	}
	return ValueToConfig(v, r.configNodes())
}

func (r *ConfigResource) deviceValue() (tftypes.Value, error) {
	var raw rawConfiguration
	if err := r.client.MarshalConfig(&raw); err != nil {
		return tftypes.Value{}, err
	}
	var doc bytes.Buffer
	doc.WriteString("<configuration>")
	doc.Write(raw.Inner)
	doc.WriteString("</configuration>")
	tree, err := patch.BuildTree(doc.Bytes())
	if err != nil {
		return tftypes.Value{}, err
	}
	return ConfigToValue(tree, r.configNodes(), r.tfType)
}

// readState returns the device's configuration as the resource's state, with
// list entries in the order they have in reference (the plan or prior state)
// where the device's order is not significant, and reference's resource_name.
func (r *ConfigResource) readState(reference tftypes.Value) (tftypes.Value, error) {
	device, err := r.deviceConfig()
	if err != nil {
		return tftypes.Value{}, err
	}
	refRoot, err := ValueToConfig(reference, r.configNodes())
	if err != nil {
		return tftypes.Value{}, err
	}
	deviceXML, err := patch.MarshalTree(device)
	if err != nil {
		return tftypes.Value{}, err
	}
	refXML, err := patch.MarshalTree(refRoot)
	if err != nil {
		return tftypes.Value{}, err
	}
	aligned, err := patch.AlignXMLOrderToReference(deviceXML, refXML, r.idx)
	if err != nil {
		return tftypes.Value{}, err
	}
	tree, err := patch.BuildTree(aligned)
	if err != nil {
		return tftypes.Value{}, err
	}
	observed, err := ConfigToValue(tree, r.configNodes(), r.tfType)
	if err != nil {
		return tftypes.Value{}, err
	}
	return r.withResourceName(observed, reference)
}

// withResourceName sets v's resource_name (Terraform's name for the resource,
// not device configuration) to reference's.
func (r *ConfigResource) withResourceName(v, reference tftypes.Value) (tftypes.Value, error) {
	var attrs map[string]tftypes.Value
	if err := v.As(&attrs); err != nil {
		return tftypes.Value{}, err
	}
	attrs["resource_name"] = tftypes.NewValue(tftypes.String, nil)
	if !reference.IsNull() {
		var refAttrs map[string]tftypes.Value
		if err := reference.As(&refAttrs); err != nil {
			return tftypes.Value{}, err
		}
		if name, ok := refAttrs["resource_name"]; ok {
			attrs["resource_name"] = name
		}
	}
	return tftypes.NewValue(r.tfType, attrs), nil
}

func (r *ConfigResource) resourceName(v tftypes.Value) string {
	var attrs map[string]tftypes.Value
	if v.IsNull() || v.As(&attrs) != nil {
		return ""
	}
	var name string
	if n, ok := attrs["resource_name"]; ok && n.IsKnown() && !n.IsNull() {
		_ = n.As(&name)
	}
	return name
}

// load merges a configuration into the device's candidate configuration.
func (r *ConfigResource) load(root *patch.Node) error {
	var inner bytes.Buffer
	for _, c := range root.Children {
		b, err := patch.MarshalTree(c)
		if err != nil {
			return err
		}
		inner.Write(b)
	}
	return r.client.SendDirectTransaction(rawConfiguration{Inner: inner.Bytes()}, false)
}

func debugPatchUpdate(resourceName string, device, plan *patch.Node, diff map[string]patch.Change, patchPayload string) {
	if os.Getenv("JUNOS_TF_DEBUG_PATCH") == "" {
		return
	}
	deviceXML, _ := patch.MarshalTree(device)
	planXML, _ := patch.MarshalTree(plan)
	fmt.Printf("\n=== terraform diff patch debug: %s ===\n", resourceName)
	fmt.Printf("--- state xml ---\n%s\n", deviceXML)
	fmt.Printf("--- plan xml ---\n%s\n", planXML)
	fmt.Printf("--- diff map ---\n")
	for _, entry := range patch.DebugSortedChanges(diff) {
		fmt.Printf("%v | %s | old=%q | new=%q\n", entry.Op, entry.Path, entry.OldVal, entry.NewVal)
	}
	fmt.Printf("--- patch payload ---\n%s\n", patchPayload)
}
