package generic

import (
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func schemaDiags(r *ConfigResource) (resource.SchemaResponse, string) {
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	var msgs []string
	for _, d := range resp.Diagnostics.Errors() {
		msgs = append(msgs, d.Summary()+": "+d.Detail())
	}
	return resp, strings.Join(msgs, "\n")
}

func TestNewConfigResourceFromSchema(t *testing.T) {
	ResetSchema()
	defer ResetSchema()
	r := NewConfigResourceFromSchema(gzipped(t, `{"root": {"children": [{"name": "configuration", "type": "container", "children": [
		{"name": "system", "type": "container", "children": [{"name": "host-name", "type": "leaf"}]}]}]}}`))
	resp, errs := schemaDiags(r)
	if errs != "" {
		t.Fatalf("unexpected errors: %s", errs)
	}
	if _, ok := resp.Schema.Attributes["system"]; !ok {
		t.Fatalf("schema has no system attribute: %v", resp.Schema.Attributes)
	}
}

// A schema that does not load is reported, not turned into a provider with no
// resources.
func TestNewConfigResourceFromSchemaReportsLoadError(t *testing.T) {
	ResetSchema()
	defer ResetSchema()
	_, errs := schemaDiags(NewConfigResourceFromSchema([]byte(`{"root": `)))
	if !strings.Contains(errs, "Invalid embedded schema") {
		t.Fatalf("expected an embedded schema error, got %q", errs)
	}
}

func TestNewConfigResourceFromSchemaReportsNameCollision(t *testing.T) {
	ResetSchema()
	defer ResetSchema()
	_, errs := schemaDiags(NewConfigResourceFromSchema([]byte(`{"root": {"children": [{"name": "configuration", "type": "container", "children": [
		{"name": "a-b", "type": "leaf"}, {"name": "a_b", "type": "leaf"}]}]}}`)))
	if !strings.Contains(errs, `both map to attribute "a_b"`) {
		t.Fatalf("expected a collision error, got %q", errs)
	}
}
