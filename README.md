# mcp-manager

`mcp-manager` keeps one central JSON configuration of MCP servers and generates
the MCP sections used by Codex, Claude Code, and OpenCode. The central file,
optionally combined with a machine-specific local override file, is the sole
source of truth; agent configs are generated outputs. It supports local
`stdio` servers, remote streamable HTTP servers, global activation,
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
`--config` to choose another file. Machine-specific settings go into
`config.local.json` next to it; see [Local overrides](#local-overrides).

```bash
# Start in your project. No config file or JSON editing is required.
cd /path/to/my-project
mcp-manager project add

# Add and enable a local MCP for this project.
mcp-manager add filesystem -- npx -y @modelcontextprotocol/server-filesystem "$PWD"

# Add a remote MCP for this project.
mcp-manager add docs --url https://mcp.example.com/mcp

# Make it available everywhere, then bring it back to this project.
mcp-manager move docs --global
mcp-manager move docs --local

# Discover this repository's worktrees and sync their MCPs too.
mcp-manager worktrees enable

mcp-manager list
mcp-manager show docs
mcp-manager sync --dry-run
```

Adding, enabling, disabling, moving, removing, importing, and changing worktree
discovery automatically sync the generated agent configs. Project registration
only updates the registry, so you can register a folder before importing its
existing MCPs. Run `sync` after editing JSON by hand or creating a new worktree.

## Everyday commands

Commands that change MCP scope use the current registered project unless you
select a scope with `--global` or `--project ID|PATH`. `--local` explicitly
selects the current project. Subdirectories select the nearest registered
parent project; an unregistered worktree selects its owning project when that
project's worktree discovery is enabled. The exception is `import`, which
defaults to global for compatibility; use `import --local` for the current
project. Flags may appear
before or after positional arguments. Use `--` before a stdio server command
to keep its flags separate from manager flags.

| Task | Command |
| --- | --- |
| Register the current folder | `mcp-manager project add` |
| Register another folder with a chosen ID | `mcp-manager project add /path/to/api --name api` |
| List registered projects | `mcp-manager project list` |
| Inspect the current project | `mcp-manager project show` |
| Stop tracking the current project | `mcp-manager project remove` |
| List MCP definitions and their scopes | `mcp-manager list` |
| List MCPs available to this project | `mcp-manager list --local` |
| List global MCPs | `mcp-manager list --global` |
| Inspect an MCP with environment and header values redacted | `mcp-manager show docs` |
| Add a project MCP | `mcp-manager add docs --url https://mcp.example.com/mcp` |
| Add a global MCP | `mcp-manager add docs --global --url https://mcp.example.com/mcp` |
| Enable an existing MCP here | `mcp-manager enable docs` |
| Disable its assignment here | `mcp-manager disable docs` |
| Make an MCP global | `mcp-manager move docs --global` |
| Make a global MCP local to this project | `mcp-manager move docs --local` |
| Disable one agent in this scope | `mcp-manager disable docs --agent claude` |
| Enable that agent again | `mcp-manager enable docs --agent claude` |
| Delete an MCP definition and all its assignments | `mcp-manager remove docs` |
| Enable worktree discovery here | `mcp-manager worktrees enable` |

All commands that change configuration accept `--dry-run` to preview their
changes without writing files. `--config PATH` and `--config-local PATH` select
the central and machine-specific files. These common flags also work before
the command, for example `mcp-manager --config /tmp/mcps.json list`.
**`--local` selects project scope; it does not select the local override file.**

`list` shows every definition, including inactive MCPs; `list --all` makes that
explicit. `list --global` shows global activations. `list --local` or
`list --project ID|PATH` shows global MCPs together with that project's local
assignments. The scope column also shows any agent exclusions.

### Register and inspect projects

`project add [PATH]` defaults to the current directory. The default project ID
comes from the folder name; use `--name ID` to choose your own. Paths are stored
as absolute paths. `project show [PROJECT]` and `project remove [PROJECT]`
accept a registered ID or path and default to the current project.

Removing a project removes its registration and project assignments. It keeps
the reusable MCP definitions and leaves generated files in that folder in
place. Project registry commands do not sync agent files.

### Add or update an MCP

Define a stdio server by putting its executable and arguments after `--`:

```bash
mcp-manager add filesystem -- npx -y @modelcontextprotocol/server-filesystem "$PWD"
mcp-manager add github --env-from GITHUB_PERSONAL_ACCESS_TOKEN -- \
  docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server
```

Define a remote HTTP server with `--url`:

```bash
mcp-manager add docs --url https://mcp.example.com/mcp \
  --header X-Client=mcp-manager --header-from Authorization=MCP_AUTHORIZATION
```

Repeat `--env KEY=VALUE` for literal stdio environment values and
`--env-from NAME` for variables forwarded from the environment. For HTTP,
repeat `--header KEY=VALUE` for literal headers and
`--header-from HEADER=ENV` for environment references. Prefer environment
references for credentials so they do not appear in shell history.

`add` saves the reusable definition and enables it in the selected scope.
Use `--global` or `--project ID|PATH` to select another scope. Repeat
`--disabled-agent AGENT` to exclude agents from that activation:

```bash
mcp-manager add docs --url https://mcp.example.com/mcp --disabled-agent claude
```

To replace an existing definition, pass `--replace` and provide the complete
new definition; the replacement affects every scope using that name:

```bash
mcp-manager add docs --replace --url https://mcp.example.com/v2/mcp
```

`show NAME` displays the definition and assignments with literal environment
and header values redacted. `remove NAME` deletes the definition and every
global and project assignment; use `disable NAME` to remove only one scope's
assignment.

### Change scope or agent access

`enable NAME` and `disable NAME` add or remove an assignment in the selected
scope. Use `--global` for the global assignment or `--project ID|PATH` for a
specific project. They preserve the definition and assignments elsewhere.
Adding `--agent codex`, `--agent claude`, or `--agent opencode` changes that
agent's exclusion in the selected scope instead of removing the assignment.
If the MCP has no assignment in that scope, `enable NAME --agent AGENT`
activates it for only that agent. On an existing assignment, it enables the
selected agent and preserves the other agents' settings.

```bash
mcp-manager enable filesystem --project api
mcp-manager disable filesystem --agent claude
mcp-manager enable filesystem --agent claude
mcp-manager disable docs --global
```

A global MCP is available everywhere. Project-scoped `enable` and `disable`
edit only the project assignment, including when a global assignment exists.
The CLI reports when global activation still applies. To restrict a global
MCP to a project, use `move NAME --local`; to change its global activation,
use `--global`.

`move NAME --global` moves the selected project's assignment into global
scope, preserving assignments in other projects. Use `enable NAME --global`
to add a global assignment while keeping the selected project's assignment.

`move NAME --local` removes the global assignment and enables the MCP in the
current project. Assignments in other projects remain. Global agent exclusions
move with the assignment unless the target project already has its own
assignment, whose exclusions take precedence. Use `--project ID|PATH` to move
into another project.

The previous positional forms, such as `enable NAME PROJECT`,
`disable NAME PROJECT`, and `move NAME PROJECT`, remain accepted.

## Central configuration reference

The CLI creates and updates this configuration for you. You can also edit it
directly and run `mcp-manager sync`:

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

## Local overrides

A second file next to the central config, `config.local.json` for
`config.json`, holds settings that belong to one machine. Set
`MCP_MANAGER_CONFIG_LOCAL` or pass `--config-local PATH` to any command to
choose another file. The local file is optional; when it does not exist,
nothing changes.

The local file is a JSON merge patch ([RFC 7386](https://www.rfc-editor.org/rfc/rfc7386))
applied to the central config: objects merge key by key, any other value
replaces the central value, and `null` removes a key. It uses the same layout
as the central config, but every field is optional and `version`, if present,
must be `2`. Arrays such as `global.mcps` and a project's `mcps` list are
replaced as a whole, so a local file that changes one of them must list every
name it wants active.

```json
{
  "projects": {
    "api": {"path": "/mnt/work/api"},
    "scratch": {"path": "~/scratch", "mcps": ["filesystem"]}
  },
  "mcps": {
    "docs": null,
    "filesystem": {"args": ["-y", "@modelcontextprotocol/server-filesystem", "/mnt/work"]}
  }
}
```

This overrides one project's path, registers a project that exists only on
this machine, removes the `docs` MCP, and changes the arguments of
`filesystem` while keeping its other fields. Only the combined result is
validated, so the central config may list projects whose directories exist on
another machine as long as the local file corrects or removes them here. That
keeps the central file shareable, for example in a dotfiles repository.

When the local file exists, it receives every configuration change made by
the CLI, including project registration, MCP definitions, scope changes,
imports, and worktree discovery. The central config is left untouched as the
shared defaults. `mcp-manager` rewrites
the local file as the patch that turns the central config into the edited result,
keeping entries that still
match the central values so that a deliberate pin survives. Create a local
file containing `{}` to start recording changes locally. Without a local
file, those commands write to the central config. The scope flag `--local`
means the current project and does not control which config file is written.

Generated wrapper entries include `--config-local PATH` when the local file is
not the default sibling of the central config, so `mcp-manager stdio` applies
the same overrides at launch. The local file has no JSON Schema because it
describes a partial document; do not point its `$schema` at
`mcp-manager.schema.json`.

## Git worktrees

Worktree discovery is disabled by default and enabled separately for each
registered project path:

```bash
# Run from the registered repository.
mcp-manager worktrees enable
mcp-manager worktrees enable --dry-run
mcp-manager worktrees disable

# Or select a project from anywhere.
mcp-manager worktrees enable --project api
```

These commands set the selected project's `includeWorktrees` to `true` or
`false` and immediately sync. A positional project ID, such as
`worktrees enable api`, is still accepted. They accept `--config PATH`, `--config-local PATH`, and
`--dry-run`; dry runs preview configuration and generated-file changes without
writing. When a local override file exists, the setting is saved there.
You can also edit `includeWorktrees` in JSON and run `mcp-manager sync`.

An enabled project's `path` must be a Git repository or worktree root. During
each sync, `git worktree list --porcelain -z` discovers its worktrees, which
inherit the project's `mcps` and `disabledAgents`. Bare entries and missing or
prunable worktree entries are skipped. A worktree registered as its
own project uses that project's settings. If multiple enabled projects would
supply settings to the same unregistered worktree, sync fails; enable discovery
for one project in that repository. Invalid enabled paths or Git failures also
stop sync before any generated files are written.

Run sync after creating a worktree and before starting the agent; discovery
does not watch for new worktrees. Disabling discovery stops syncing inherited
worktrees but does not delete their previously generated files. MCP commands,
arguments, and environment values are inherited unchanged, including any
absolute paths in server definitions.

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
  `--config /absolute/path` so the wrapper reads the same file. A local
  override file outside its default location is embedded as `--config-local`
  in the same way.
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
mcp-manager import --from codex --global

# Register this folder, then import its existing Claude Code MCPs.
mcp-manager project add
mcp-manager import --from claude --local

# Import another registered project's config.
mcp-manager import --from claude --project api

# Preview the import and generated changes without writing files.
mcp-manager import --from opencode --local --dry-run
```

`--from` accepts `codex`, `claude`, or `opencode`. `--local` imports the current
project's file; `--project ID|PATH` selects a registered project. The previous
`--project ID=PATH` form still registers a project and imports its file in one
step. Without a scope flag, import reads the selected agent's global file for
compatibility. Imported
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
