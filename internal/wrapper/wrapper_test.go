package wrapper

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

const helperEnv = "MCP_MANAGER_WRAPPER_TEST_HELPER"

// TestMain lets the test binary act as a tiny stdio server when helperEnv is
// set, so Run can be exercised without depending on external executables.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "1" {
		os.Exit(m.Run())
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	fmt.Fprint(os.Stdout, strings.ToUpper(string(input)))
	fmt.Fprintf(os.Stderr, "args=%s\n", strings.Join(os.Args[1:], ","))
	fmt.Fprintf(os.Stderr, "forwarded=%s\n", os.Getenv("HELPER_FORWARDED"))
	fmt.Fprintf(os.Stderr, "literal=%s\n", os.Getenv("HELPER_LITERAL"))
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

func TestArgsAndParseRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		configPath string
		localPath  string
		want       []string
	}{
		{name: "tools", want: []string{"stdio", "tools"}},
		{name: "tools", configPath: "/central/config.json", want: []string{"stdio", "--config", "/central/config.json", "tools"}},
		{name: "tools", localPath: "/central/overrides.json", want: []string{"stdio", "--config-local", "/central/overrides.json", "tools"}},
		{
			name: "tools", configPath: "/central/config.json", localPath: "/central/overrides.json",
			want: []string{"stdio", "--config", "/central/config.json", "--config-local", "/central/overrides.json", "tools"},
		},
	}
	for _, test := range tests {
		got := Args(test.name, test.configPath, test.localPath)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("Args(%q, %q, %q) = %#v, want %#v", test.name, test.configPath, test.localPath, got, test.want)
		}
		for _, command := range []string{Command, "/usr/local/bin/mcp-manager", `C:\Tools\mcp-manager.exe`} {
			name, ok := Parse(command, got)
			if !ok || name != test.name {
				t.Fatalf("Parse(%q, %#v) = %q, %v; want %q, true", command, got, name, ok, test.name)
			}
		}
	}
	inlineForms := [][]string{
		{"stdio", "--config=/central/config.json", "tools"},
		{"stdio", "--config-local=/central/overrides.json", "-config", "/central/config.json", "tools"},
	}
	for _, args := range inlineForms {
		if name, ok := Parse(Command, args); !ok || name != "tools" {
			t.Fatalf("Parse(%#v) = %q, %v; want tools, true", args, name, ok)
		}
	}
}

func TestParseRejectsForeignCommandLines(t *testing.T) {
	tests := []struct {
		command string
		args    []string
	}{
		{command: "npx", args: []string{"stdio", "tools"}},
		{command: "mcp-manager-helper", args: []string{"stdio", "tools"}},
		{command: Command, args: nil},
		{command: Command, args: []string{"sync"}},
		{command: Command, args: []string{"stdio"}},
		{command: Command, args: []string{"stdio", "--config"}},
		{command: Command, args: []string{"stdio", "--config", "/path"}},
		{command: Command, args: []string{"stdio", "--config-local"}},
		{command: Command, args: []string{"stdio", "--config-local", "/path"}},
		{command: Command, args: []string{"stdio", "--config", "/path", "--verbose", "tools"}},
		{command: Command, args: []string{"stdio", "tools", "extra"}},
		{command: Command, args: []string{"stdio", "--dry-run", "tools"}},
	}
	for _, test := range tests {
		if name, ok := Parse(test.command, test.args); ok {
			t.Fatalf("Parse(%q, %#v) = %q, true; want no match", test.command, test.args, name)
		}
	}
}

func TestPrepareResolvesCommandAndEnvironment(t *testing.T) {
	cfg := &config.Config{
		Version: config.CurrentVersion,
		MCPs: map[string]config.MCP{
			"tools": {
				Type: "stdio", Command: os.Args[0], Args: []string{"--flag", "value"},
				Env:     map[string]string{"HELPER_LITERAL": "from-config", "SHARED": "override"},
				EnvFrom: []string{"HELPER_FORWARDED"},
			},
		},
	}
	environ := []string{"SHARED=inherited", "HELPER_FORWARDED=secret", "OTHER=kept"}

	launch, err := Prepare(cfg, "tools", environ)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if launch.Path == "" {
		t.Fatal("Prepare() returned an empty path")
	}
	if want := []string{os.Args[0], "--flag", "value"}; !reflect.DeepEqual(launch.Args, want) {
		t.Fatalf("Args = %#v, want %#v", launch.Args, want)
	}
	want := []string{"HELPER_FORWARDED=secret", "OTHER=kept", "HELPER_LITERAL=from-config", "SHARED=override"}
	if !reflect.DeepEqual(launch.Env, want) {
		t.Fatalf("Env = %#v, want %#v", launch.Env, want)
	}
	if !reflect.DeepEqual(environ, []string{"SHARED=inherited", "HELPER_FORWARDED=secret", "OTHER=kept"}) {
		t.Fatalf("Prepare() mutated the caller's environment: %#v", environ)
	}
}

func TestPrepareRejectsInvalidLaunches(t *testing.T) {
	cfg := &config.Config{
		Version: config.CurrentVersion,
		MCPs: map[string]config.MCP{
			"remote":  {Type: "http", URL: "https://example.com/mcp"},
			"needs":   {Type: "stdio", Command: os.Args[0], EnvFrom: []string{"WRAPPER_TEST_MISSING"}},
			"missing": {Type: "stdio", Command: "mcp-manager-test-command-that-does-not-exist"},
		},
	}
	tests := []struct {
		name    string
		mcp     string
		wantErr string
	}{
		{name: "unknown", mcp: "nope", wantErr: `MCP "nope" does not exist`},
		{name: "http transport", mcp: "remote", wantErr: "only stdio MCPs can be launched"},
		{name: "unset envFrom", mcp: "needs", wantErr: `requires environment variable "WRAPPER_TEST_MISSING"`},
		{name: "unknown command", mcp: "missing", wantErr: `locate command "mcp-manager-test-command-that-does-not-exist"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Prepare(cfg, test.mcp, []string{"PATH=" + os.Getenv("PATH")})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Prepare() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
	if _, err := Prepare(nil, "tools", nil); err == nil {
		t.Fatal("Prepare(nil) error = nil, want error")
	}
}

func TestRunRelaysStreamsEnvironmentAndExitCode(t *testing.T) {
	t.Setenv("HELPER_FORWARDED", "forwarded-secret")
	cfg := &config.Config{
		Version: config.CurrentVersion,
		MCPs: map[string]config.MCP{
			"tools": {
				Type: "stdio", Command: os.Args[0], Args: []string{"first", "second"},
				Env:     map[string]string{helperEnv: "1", "HELPER_EXIT": "3", "HELPER_LITERAL": "literal-value"},
				EnvFrom: []string{"HELPER_FORWARDED"},
			},
		},
	}
	launch, err := Prepare(cfg, "tools", os.Environ())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	code, err := Run(launch, strings.NewReader("ping"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 3 {
		t.Fatalf("Run() code = %d, want 3", code)
	}
	if stdout.String() != "PING" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "PING")
	}
	for _, line := range []string{"args=first,second", "forwarded=forwarded-secret", "literal=literal-value"} {
		if !strings.Contains(stderr.String(), line) {
			t.Fatalf("stderr = %q, want line %q", stderr.String(), line)
		}
	}
}

func TestRunRejectsEmptyLaunch(t *testing.T) {
	if code, err := Run(Launch{}, strings.NewReader(""), io.Discard, io.Discard); err == nil || code != 1 {
		t.Fatalf("Run(Launch{}) = %d, %v; want 1 and error", code, err)
	}
}
