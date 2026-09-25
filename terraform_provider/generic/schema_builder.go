package generic

import (
	"fmt"
	"regexp"
	"strings"

	"terraform_provider/patch"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// SanitizeName converts a YANG node name to a valid Terraform attribute name:
// lowercase letters, digits and underscores. A few Junos names have capitals
// (AH_header, ESP_header in firewall filters).
func SanitizeName(name string) string {
	return strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

var validAttributeName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ValidateNames checks that every node's attribute name is valid and that no
// two siblings map to the same one, so the schema cannot silently drop a node.
func ValidateNames(nodes []patch.SchemaNode, path string) error {
	seen := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		key := SanitizeName(n.Name)
		if !validAttributeName.MatchString(key) {
			return fmt.Errorf("%s/%s: %q is not a valid attribute name", path, n.Name, key)
		}
		if other, ok := seen[key]; ok {
			return fmt.Errorf("%s: %s and %s both map to attribute %q", path, other, n.Name, key)
		}
		seen[key] = n.Name
		if err := ValidateNames(n.Children, path+"/"+n.Name); err != nil {
			return err
		}
	}
	return nil
}

// BuildSchema produces a schema.Schema from the pyang JSON SchemaNode tree.
func BuildSchema(roots []patch.SchemaNode) schema.Schema {
	// roots[0] is the "configuration" node; use its children.
	var configChildren []patch.SchemaNode
	if len(roots) > 0 {
		configChildren = roots[0].Children
	}

	attrs := buildAttributes(configChildren)
	attrs["resource_name"] = schema.StringAttribute{
		Required:      true,
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}

	return schema.Schema{Attributes: attrs}
}

// attributeNodes returns the schema nodes that are Terraform attributes, in
// schema order. The schema builder and the value converters both use it, so
// they agree on what the resource holds.
func attributeNodes(nodes []patch.SchemaNode) []patch.SchemaNode {
	out := make([]patch.SchemaNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Name == "" || n.Name == "groups" || n.Name == "apply-groups" {
			continue
		}
		switch n.Type {
		case "leaf", "leaf-list", "container", "list":
			out = append(out, n)
		}
	}
	return out
}

func buildAttributes(nodes []patch.SchemaNode) map[string]schema.Attribute {
	attrs := make(map[string]schema.Attribute, len(nodes))
	for _, n := range attributeNodes(nodes) {
		key := SanitizeName(n.Name)
		switch n.Type {
		case "leaf":
			attrs[key] = schema.StringAttribute{Optional: true}
		case "leaf-list":
			attrs[key] = schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
			}
		case "container", "list":
			nested := buildAttributes(n.Children)
			attrs[key] = schema.ListNestedAttribute{
				Optional: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: nested,
				},
			}
		}
	}
	return attrs
}
