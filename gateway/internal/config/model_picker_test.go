package config

import (
	"strings"
	"testing"
)

func modelPickerPolicyClient(shape string) Client {
	return Client{
		Name:          "claude-code",
		ProtocolShape: shape,
		ModelAliases: map[string]string{
			"claude-baseten-alpha": "example/Alpha",
			"claude-baseten-beta":  "example/Beta",
		},
		ModelPicker: &ModelPicker{
			Enabled: true,
			Models: []ModelPickerModel{
				{Alias: "claude-baseten-alpha"},
				{Alias: "claude-baseten-beta"},
			},
		},
	}
}

func TestValidateModelPicker(t *testing.T) {
	enabled := true
	base := func() *File {
		return &File{
			Global:  Global{RoutingEnabled: &enabled},
			Clients: []Client{modelPickerPolicyClient("anthropic")},
		}
	}

	t.Run("valid ordered rows", func(t *testing.T) {
		if err := ValidateRoutingPolicy(base()); err != nil {
			t.Fatalf("ValidateRoutingPolicy() error = %v", err)
		}
	})

	cases := []struct {
		name string
		edit func(*Client)
		want string
	}{
		{
			name: "wrong protocol shape",
			edit: func(c *Client) { c.ProtocolShape = "openai" },
			want: "requires protocol_shape anthropic",
		},
		{
			name: "missing alias",
			edit: func(c *Client) { c.ModelPicker.Models[0].Alias = "claude-baseten-missing" },
			want: "missing from model_aliases",
		},
		{
			name: "duplicate alias",
			edit: func(c *Client) { c.ModelPicker.Models[1].Alias = c.ModelPicker.Models[0].Alias },
			want: "is duplicated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := base()
			tc.edit(&file.Clients[0])
			err := ValidateRoutingPolicy(file)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRoutingPolicy() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestModelPickerStrictParsing(t *testing.T) {
	const prefix = `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-alpha: example/Alpha
`
	cases := []struct {
		name string
		tail string
		want string
	}{
		{
			name: "null subtree",
			tail: "    model_picker: null\n",
			want: "model_picker must be an object, not null",
		},
		{
			name: "unknown row field",
			tail: "    model_picker:\n      enabled: true\n      models:\n        - alias: claude-baseten-alpha\n          custom_label: no\n",
			want: "field custom_label not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var file File
			err := UnmarshalStrict([]byte(prefix+tc.tail), &file)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UnmarshalStrict() error = %v, want substring %q", err, tc.want)
			}
		})
	}

	var absent File
	if err := UnmarshalStrict([]byte(prefix), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Clients[0].ModelPicker != nil {
		t.Fatalf("absent model_picker parsed as %#v", absent.Clients[0].ModelPicker)
	}
	var disabled File
	if err := UnmarshalStrict([]byte(prefix+"    model_picker:\n      enabled: false\n      models: []\n"), &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.Clients[0].ModelPicker == nil || disabled.Clients[0].ModelPicker.Enabled {
		t.Fatalf("explicit disabled model_picker parsed as %#v", disabled.Clients[0].ModelPicker)
	}
}
