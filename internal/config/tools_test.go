package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestToolPoliciesAreScopedAndLayered(t *testing.T) {
	root := t.TempDir()
	central := filepath.Join(root, "config.json")
	cfg := &Config{Version: CurrentVersion, Global: Scope{MCPs: []string{"api"}, DisabledTools: map[string][]string{"api": {"delete"}}}, Projects: map[string]Project{"project": {Path: root, MCPs: []string{"api"}, DisabledTools: map[string][]string{"api": {"write"}}}}, MCPs: map[string]MCP{"api": {Type: "http", URL: "https://example.test/mcp"}}}
	if err := Save(central, cfg); err != nil {
		t.Fatal(err)
	}
	local := LocalPathFor(central)
	if err := os.WriteFile(local, []byte(`{"projects":{"project":{"disabledTools":{"api":["read"]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(central)
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Path: central, LocalPath: local}
	edit, err := source.OpenEdit()
	if err != nil {
		t.Fatal(err)
	}
	for project, want := range map[string][]string{"": {"delete"}, "project": {"read"}} {
		got, err := edit.Config.ToolExclusions("api", project)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("policy %q = %v, %v", project, got, err)
		}
	}
	edit.Config.Projects["project"].DisabledTools["api"] = []string{"other"}
	if err := edit.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(central)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("local edit changed central config")
	}
	effective, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effective.Projects["project"].DisabledTools["api"], []string{"other"}) {
		t.Fatal("tool exclusions did not round trip")
	}
}

func TestInvalidToolPolicies(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{`null`, "must be an object"},
		{`{"api":null}`, "must be an array"},
		{`{"api":[1]}`, "cannot unmarshal"},
		{`{"api":[""]}`, "invalid tool name"},
		{`{"api":["x","x"]}`, "duplicate tool name"},
		{`{"api":["bad\nname"]}`, "invalid tool name"},
		{`{"missing":["x"]}`, "not active"},
	} {
		t.Run(test.value, func(t *testing.T) {
			data := `{"version":2,"global":{"mcps":["api"],"disabledTools":` + test.value + `},"projects":{},"mcps":{"api":{"type":"http","url":"https://example.test"}}}`
			_, err := Parse([]byte(data), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse = %v; want %s", err, test.want)
			}
		})
	}
	// Tool names are exact names, and need not follow MCP server ID syntax.
	tools := []string{"read.file", "api/tool", "tool:one"}
	raw, _ := json.Marshal(tools)
	if _, err := Parse([]byte(`{"version":2,"global":{"mcps":["api"],"disabledTools":{"api":`+string(raw)+`}},"projects":{},"mcps":{"api":{"type":"stdio","command":"server"}}}`), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
