# mcp-manager

`mcp-manager` keeps one central JSON configuration of MCP servers and generates
the MCP sections used by Codex, Claude Code, and OpenCode. The central file is
the sole source of truth; agent configs are generated outputs. It supports
local `stdio` servers, remote streamable HTTP servers, global activation,
per-project activation, per-scope agent exclusions, and optional secret
materialization. Generated `stdio` entries launch `mcp-manager stdio NAME`,
so agents always start local servers through `mcp-manager` and pick up the
central definition at launch time.

## Build and install

Go 1.24 or newer is required to build from source.

```bash
mkdir -p ~/.local/bin
go test ./...
go build -trimpath -o ~/.local/bin/mcp-manager ./cmd/mcp-manager
```

Tagged GitHub releases contain standalone Linux, macOS, and Windows binaries
for AMD64 and ARM64. Push a semantic version tag such as `v0.1.0` to run the
release workflow.

## Quick start

The default central config is `<user-config-dir>/mcp-manager/config.json`
(`~/.config/mcp-manager/config.json` on Linux). Set `MCP_MANAGER_CONFIG` or use
`--config` to choose another file.

```bash
mkdir -p ~/.config/mcp-manager
cp mcp-manager.example.json ~/.config/mcp-manager/config.json
cp mcp-manager.schema.json ~/.config/mcp-manager/mcp-manager.schema.json
$EDITOR ~/.config/mcp-manager/config.json

mcp-manager sync --dry-run
mcp-manager sync
```

The central format is deliberately small:

```json
{
  "$schema": "./mcp-manager.schema.json",
  "version": 2,
  "options": {
    "inlineSecrets": false,
    "stdioMode": "wrapper"
  },
  "global": {
    "mcps": ["docs"],
    "disabledAgents": {
      "docs": ["opencode"]
    }
  },
  "projects": {
    "api": {
      "path": "/home/me/code/api",
      "mcps": ["filesystem", "docs"],
      "disabledAgents": {
        "filesystem": ["claude"],
        "docs": ["codex"]
      }
    }
  },
  "mcps": {
    "filesystem": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/me/code"],
      "env": {"LOG_LEVEL": "info"},
      "envFrom": ["GITHUB_TOKEN"]
    },
    "docs": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": {"X-Client": "mcp-manager"},
      "headersFrom": {"Authorization": "MCP_AUTHORIZATION"}
    }
  }
}
```

Project IDs and MCP names may contain letters, digits, `_`, and `-`. Project
`path` values must point to existing directories and be absolute; a leading
`~/` is expanded. The root `mcps` object contains reusable transport
definitions only. An MCP is activated by listing its name in `global.mcps` or
in a project's `mcps` list.

Each activation scope has its own `disabledAgents` map from MCP name to an
array containing `codex`, `claude`, and/or `opencode`. An exclusion applies
only to the scope containing that map: `global.disabledAgents` controls global
outputs, while `projects.<id>.disabledAgents` controls only that project's
outputs. Every map key must also appear in that scope's `mcps` list. This
allows the same transport to have different agent exclusions in different
scopes.

The complete machine-readable definition is in
[`mcp-manager.schema.json`](./mcp-manager.schema.json), with a ready-to-edit
configuration in [`mcp-manager.example.json`](./mcp-manager.example.json).

## Launching stdio servers through mcp-manager

By default, every generated `stdio` entry runs the `mcp-manager` binary
instead of the server's real command:

```json
{
  "mcpServers": {
    "filesystem": {
      "type": "stdio",
      "command": "mcp-manager",
      "args": ["stdio", "filesystem"],
      "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}
    }
  }
}
```

When an agent starts the server, `mcp-manager stdio filesystem` loads the
central config, looks up the transport definition, applies its literal `env`
values, checks that every `envFrom` variable is present in the environment,
and replaces itself with the real command through `exec`. The agent therefore
talks to the server process directly, signals and the exit status pass through
unchanged, and stdout is never touched by `mcp-manager`; diagnostics go to
stderr only. On Windows, where `exec` is unavailable, the wrapper stays
running as the parent process, relays the standard streams, and exits with
the server's exit code.

Because the real command, arguments, and literal environment are resolved at
launch, editing them in the central config takes effect the next time an
agent starts the server, without running `sync`. Renaming, adding, removing,
or re-scoping an MCP still requires `sync`, as does changing `envFrom`.

Requirements and details:

- The `mcp-manager` binary must be on the `PATH` used by Codex, Claude Code,
  and OpenCode. The build instructions above install it to `~/.local/bin`.
- When `sync` runs with a config outside the default location, whether through
  `--config` or `MCP_MANAGER_CONFIG`, the generated entries include
  `--config /absolute/path` so the wrapper reads the same file.
- `envFrom` references are still generated in each agent's native form so the
  agent forwards those variables to the wrapper. Codex in particular starts
  MCP servers with a minimal environment and only passes the variables listed
  in `env_vars`. Literal `env` values are not written to agent configs.
- `--inline-secrets` still writes resolved `envFrom` values into agent configs;
  the wrapper inherits them from the agent and forwards them to the server.
- `import` recognizes generated wrapper entries and maps them back to the
  central definition, so importing an agent config after a `sync` reports the
  MCPs as unchanged. A wrapper entry whose name is not defined in the central
  config is an error.
- `mcp-manager stdio NAME` works for every defined `stdio` MCP, whether or not
  it is activated in a scope, which makes it a convenient way to test a server
  definition by hand.

To write the real command line into agent configs instead, set the option in
the central config:

```json
{
  "options": {
    "stdioMode": "direct"
  }
}
```

`stdioMode` accepts `wrapper` (default) or `direct`. HTTP servers are never
wrapped; their entries are generated the same way in both modes.

## Generated files

| Agent | Global | Project | Managed key |
| --- | --- | --- | --- |
| Codex | `~/.codex/config.toml` | `.codex/config.toml` | `mcp_servers` |
| Claude Code | `~/.claude.json` | `.mcp.json` | `mcpServers` |
| OpenCode | `<user-config-dir>/opencode/opencode.json` | `opencode.json` | `mcp` |

OpenCode's current project filename is `opencode.json`, without a leading dot.
The `.opencode/` directory is used for other OpenCode resources.

Every sync treats the single central config as authoritative for the managed
key. Generated agent configs are outputs, not additional configuration stores.
Old MCP entries are removed, including entries excluded by `disabledAgents` in
that scope; all unrelated top-level settings remain. Before writing,
`mcp-manager` parses every existing destination and renders every result in
memory. Files are then replaced atomically one at a time, existing permissions
are retained, new files use mode `0600`, and symlink destinations are refused.

JSONC input is accepted, but generated JSON is normalized to JSON. TOML and
JSON are semantically preserved rather than textually patched, so comments and
original formatting inside an existing generated file can be lost.

## Environment references and inline secrets

Use `env` and `headers` for literal strings. Use `envFrom` to forward named
environment variables to a `stdio` server and `headersFrom` to map HTTP header
names to environment variables. In the default wrapper mode, literal `env`
values stay in the central config and are applied by `mcp-manager stdio` at
launch. Without inline mode, the generator emits each agent's native reference
form for `envFrom` and `headersFrom`:

| Target | `envFrom` | `headersFrom` |
| --- | --- | --- |
| Codex | `env_vars` | `env_http_headers` |
| Claude Code | `${VARIABLE}` | `${VARIABLE}` |
| OpenCode | `{env:VARIABLE}` | `{env:VARIABLE}` |

To resolve those variables immediately and write their current values as
literal strings, enable the config option or pass the CLI flag:

```bash
mcp-manager sync --inline-secrets
```

Inline mode fails before any write if a required variable is unset, and secret
values are never printed. It does, however, put those values directly into all
generated config files. Treat those files as secrets and do not commit them.

## Import existing MCPs

Import one agent-native config into the central config and automatically
sync it to all enabled agents:

```bash
# Import the selected agent's global MCP section.
mcp-manager import --from codex

# Register a project and import its Claude Code project MCPs.
mcp-manager import --from claude --project api=/absolute/path/to/api

# Preview the import and generated changes without writing files.
mcp-manager import --from opencode --dry-run
```

`--from` accepts `codex`, `claude`, or `opencode`. A project import requires
`ID=PATH`; without it, import reads the selected agent's global file. Imported
transport definitions are added to the root `mcps` object. Entries that launch
`mcp-manager stdio NAME` are mapped back to the central definition of `NAME`
rather than imported as literal `mcp-manager` commands. Their names are
added to `global.mcps` for a global import or to the registered project's
`mcps` list for a project import. Exact native environment references are
converted back to `envFrom` and `headersFrom`. A same-name transport conflict
aborts without changing the central file. After updating the central config,
import automatically runs sync to update the generated outputs. With
`--dry-run`, it previews both the central-config and generated-output changes
without writing any files. Import does not infer agent exclusions from the one
source agent; configure any `disabledAgents` entries in their intended scope.

Literal credentials already present in a native config remain literal when
imported. Review the central file before committing it.

## Move a global MCP into a project

Move a globally active MCP into one registered project's local scope:

```bash
mcp-manager move [--config PATH] [--dry-run] MCP PROJECT
```

`MCP` is the MCP name and `PROJECT` is a registered project ID. The command
removes the name from `global.mcps` and adds it to the project's `mcps` list.
The root transport definition and assignments to other projects are preserved.
Because exclusions are scope-specific, a matching `global.disabledAgents`
entry moves to the target project with the activation. If that project already
has its own entry for the MCP, its existing project-specific exclusions win.
Unlike `enable`, `move` disables global activation.
With `--dry-run`, the proposed central-config and generated-output changes are
shown without writing any files.

After updating the central config, move automatically runs sync to apply the
new scopes to the generated outputs:

```bash
mcp-manager move docs api
```

## Enable a non-global MCP for a project

Enable an MCP that is already non-global for another registered project:

```bash
mcp-manager enable [--config PATH] [--dry-run] MCP PROJECT
```

`enable` requires the MCP not to be listed in `global.mcps`. It adds the MCP's
name to the project's `mcps` list while preserving its root transport
definition, other project assignments, and exclusions in other scopes. It
does not create an exclusion in the new project scope, so the MCP is enabled
for all agents there by default. Unlike `move`, it does not change global
activation; use `move` when a globally active MCP should become project-only.
With `--dry-run`, the proposed central-config and generated-output changes are
shown without writing any files.

After updating the central config, enable automatically runs sync to apply
the added project scope to the generated outputs:

```bash
mcp-manager enable filesystem web
```

## Disable a non-global MCP for a project

Remove one project assignment from a non-global MCP:

```bash
mcp-manager disable [--config PATH] [--dry-run] MCP PROJECT
```

`disable` requires the MCP not to be listed in `global.mcps`. It removes only
the MCP's name from the selected project's `mcps` list, preserving the root
transport definition, assignments to other projects, and settings in other
scopes. Its `disabledAgents` entry in that project is removed with the project
activation. The command is idempotent: if the MCP is not assigned to that
project, the central config is left unchanged. With `--dry-run`, the proposed
central-config and generated-output changes are shown without writing any
files.

Under the current activation model, a global MCP is active everywhere and
cannot be disabled for only one project. After updating the central config,
disable automatically runs sync to apply the removed project scope to the
generated outputs:

```bash
mcp-manager disable filesystem web
```

## Launch a stdio MCP by hand

```bash
mcp-manager stdio [--config PATH] MCP
```

This is the command that generated agent configs run. It resolves the named
`stdio` MCP from the central config and replaces itself with the server
process, so it can be used to test a definition interactively or from another
MCP client. It fails before launching anything when the MCP is unknown, uses
the `http` transport, lists an `envFrom` variable that is not set, or names a
command that cannot be found on the `PATH`.

## Development and releases

```bash
gofmt -w cmd internal
go mod tidy
go vet ./...
go test -race ./...
go build ./cmd/mcp-manager
```

CI runs formatting, module, vet, race-test, and build checks. Tags matching
`vX.Y.Z` produce compressed binaries plus `checksums.txt` in a GitHub release.

The target layouts follow the current product documentation:

- [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)
- [Claude Code MCP scopes](https://code.claude.com/docs/en/mcp#mcp-installation-scopes)
- [OpenCode config locations](https://opencode.ai/docs/config/#locations)
- [OpenCode MCP servers](https://opencode.ai/docs/mcp-servers/)
