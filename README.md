# harness

`harness` is a Go library for driving AI coding CLIs in headless mode. It
supports Claude Code, Codex, GitHub Copilot CLI, and OpenCode through one
interface while leaving process placement to the caller. A command can run on
the host, inside a container, or through a remote runner with the same arguments
and event parser.

## Supported backends

| Name | Binary | Credential environment | Staged skill path | Project instructions | Model API hosts |
| --- | --- | --- | --- | --- | --- |
| `claude` | `claude` | `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` | `.claude/skills/<name>` | `CLAUDE.md` | `*.anthropic.com` |
| `codex` | `codex` | `CODEX_API_KEY` | `skills/<name>` | `AGENTS.md` | `api.openai.com`, `auth0.openai.com`, `chatgpt.com` |
| `copilot` | `copilot` | `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN` | `.github/skills/<name>` | `.github/copilot-instructions.md` | `github.com`, `api.github.com`, `api.mcp.github.com`, `*.githubcopilot.com` |
| `opencode` | `opencode` | `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENCODE_CONFIG_CONTENT`, `OPENCODE_AUTH_CONTENT` | `.opencode/skill/<name>` | `AGENTS.md` | `models.dev`, `api.openai.com`, `*.anthropic.com` |

The Copilot adapter targets CLI 1.0.80 and remains compatible with the
prompt-mode JSONL stream introduced in 1.0.75.

For Copilot BYOK runs, `Job.BaseURL` sets `COPILOT_PROVIDER_BASE_URL` and
`Harness.Env` passes through the configured `COPILOT_MODEL` and
`COPILOT_PROVIDER_*` credentials and wire settings as bare keys, so container
and remote runners can inject their values without placing secrets in argv.

The library owns the details that differ between CLIs: binary names, arguments,
credential and state environment variables, project instruction files, skill
directories, model API hosts, JSONL parsing, account-limit errors, default
models, and token prices.

## Install

```sh
go get github.com/alpha-omega-security/harness
```

Go 1.26 or later is required.

## Core API

A `Job` contains resolved values for one invocation. Callers apply their own
configuration defaults before constructing it.

```go
type Job struct {
    Workspace string
    SrcDir    string
    SkillName string

    Prompt       string
    SystemPrompt string

    Model    string
    Effort   string
    MaxTurns int

    OutputFile  string
    AllowedTools string
    BaseURL      string

    ResumeSessionID string
    ResumePrompt    string
}
```

`Workspace` is the command's working directory. `SrcDir` is the repository
directory relative to it and defaults to `src`; set it to `.` when `Workspace`
is already the repository root. `SkillName` selects a staged `SKILL.md`; when
`Prompt` is empty, the backend builds a short activation prompt. `SystemPrompt`
uses `--system-prompt` with Claude and the backend's project instruction file
for the other CLIs.

`MaxTurns` uses the backend default when set to zero; Copilot maps it to maximum
autopilot continuations. `Effort` applies to Claude and Copilot. `AllowedTools`
applies only to Claude. `ResumeSessionID` and `ResumePrompt` continue an
existing conversation.

The `Harness` interface exposes the parts needed by local, container, and remote
runners:

```go
type Harness interface {
    Binary() string
    Args(Job) []string
    Prompt(Job) string
    ParseStream(io.Reader, func(Event))
    SkillDir(workspace, name string) string
    GuideFilename() string
    SystemPromptViaArgs() bool
    EgressHosts() []string
    Env(baseURL string) []string
    StateEnv(dir string) []string
    AccountErrorText(string) string
    DefaultModels() []ModelDefault
}
```

Use `ByName` to select a backend. An empty name selects Claude.

```go
h, err := harness.ByName("codex")
name := harness.Name(h)
available := harness.Names() // "claude, codex, copilot, opencode"
```

Each parser produces the same event type:

```go
type Event struct {
    Kind      string
    Tool      string
    Text      string
    CostUSD   float64
    Turns     int
    Usage     Usage
    SessionID string
    RateLimit *RateLimitInfo
}
```

Kinds are `thinking`, `text`, `tool`, `result`, `error`, `session`, and
`rate_limit`. `FormatEvent` renders an event for a plain-text log.
`CostFromUsage` calculates a list-price estimate when the CLI reports tokens
without a dollar amount. Copilot's `CostUSD` uses the latest cumulative
`session.usage_checkpoint` when one is present, so on a resumed Copilot session
the reported cost is session-cumulative rather than per-invocation.

## Run a local subprocess

`Run` starts the selected binary in the workspace, applies its environment,
writes a project instruction file when needed, and parses combined output as it
arrives.

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/alpha-omega-security/harness"
    "github.com/alpha-omega-security/harness/egress"
    "github.com/alpha-omega-security/harness/skills"
)

func main() {
    ctx := context.Background()
    workspace := "/work/project"

    h, err := harness.ByName("claude")
    if err != nil {
        log.Fatal(err)
    }
    instructions, err := skills.Parse("/work/instructions/review.md")
    if err != nil {
        log.Fatal(err)
    }

    job := harness.Job{
        Workspace:    workspace,
        SrcDir:       ".",
        Prompt:       "Review this project for security defects.",
        SystemPrompt: skills.Concat(instructions),
        Model:        "claude-sonnet-4-6",
        MaxTurns:     20,
    }

    if err := egress.WriteSandboxSettings(workspace, h.EgressHosts()); err != nil {
        log.Fatal(err)
    }
    err = harness.Run(ctx, h, job, func(event harness.Event) {
        fmt.Println(harness.FormatEvent(event))
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

When a non-zero exit contains a provider account-limit message, `Run` returns
an `*harness.AccountError`. Its optional reset time can be used to schedule a
later retry.

## Use another process runner

Callers that own process creation can use the same API without `Run`. This is
useful for containers, job queues, and remote execution.

```go
h, err := harness.ByName("codex")
if err != nil {
    return err
}
skill, err := skills.Parse("/skills/security-review/SKILL.md")
if err != nil {
    return err
}
job := harness.Job{
    Workspace:  "/work",
    SrcDir:     ".",
    SkillName:  skill.Name,
    Model:      "gpt-5.3-codex",
    OutputFile: "report.json",
}
if err := skills.Stage(h, job, skill); err != nil {
    return err
}
if err := harness.WriteSystemPrompt(h, job); err != nil {
    return err
}
argv := append([]string{h.Binary()}, h.Args(job)...)
env := append(h.Env(job.BaseURL), h.StateEnv("/state")...)

// Pass argv and env to the process runner. Entries such as "CODEX_API_KEY"
// use the `docker run -e KEY` passthrough form.
stdout, err := startContainer(ctx, argv, env)
if err != nil {
    return err
}
h.ParseStream(stdout, func(event harness.Event) {
    fmt.Println(harness.FormatEvent(event))
})
```

`WriteSystemPrompt` is needed only when `SystemPrompt` is non-empty. Backends
whose `SystemPromptViaArgs` method returns true receive that value in their
arguments, so the helper does not write a guide file.

## Process isolation

The generated arguments allow unattended tool use. Claude uses
`bypassPermissions` unless `AllowedTools` is set. Codex uses
`danger-full-access`, OpenCode uses `--auto`, and Copilot uses `--allow-all`.
Run these commands only in a workspace and execution environment where those
permissions are acceptable. Container and remote callers should apply their
own filesystem, process, secret, and network limits.

The `egress` package can restrict outbound HTTP and HTTPS by hostname. Its proxy
checks the resolved destination immediately before connecting and rejects
loopback, private, link-local, carrier-grade NAT, unspecified, and multicast
addresses. This closes the usual DNS rebinding path after a hostname has passed
the allowlist.

## Packages

`skills` parses `SKILL.md` files following the [Agent Skills specification](https://agentskills.io/specification). YAML frontmatter is optional, so plain markdown instruction files parse too. `Parse` returns a `Skill` with the spec fields (name, description, license, compatibility, allowed-tools, metadata), the body, a sibling `schema.json` when present, and a content hash covering both. `Walk` finds skills under a directory; `Stage` writes one into the selected backend's discovery directory; `Concat` joins bodies for a system prompt; `Render` produces `SKILL.md` bytes from a `Skill` built in memory. `ValidateNamespace` and the `Match`/`PathIncluded` glob helpers support callers that add their own metadata keys and path filters.

`container` runs a backend inside an ephemeral OCI container. `Runner.Run`
mirrors `harness.Run`'s signature; the workspace is bind-mounted at `/work`
and an optional state directory at `/harness-state` so a later run can resume
the session. `DetectRuntime` resolves docker, podman (rootful or rootless), or
Apple's `container` CLI and applies each engine's flag differences
(`--userns=keep-id`, `--progress none`, missing `--security-opt`). SELinux
`:z` bind-mount relabeling is handled via `ResolveSELinuxRelabel`, and
`VerifyKeepID` / `VerifySELinuxMount` report host misconfiguration once during
startup. `Runner.Hardened`
creates an internal network. `Runner.Run` owns that network for one backend
invocation. Call `Runner.Open` when readiness checks or retries must share it;
the returned `Scope` runs backends with `Run`, auxiliary commands with
`RunCommand`, and removes its resources with `Close`. `Runner.ProcessEnv`
supplies scoped values for bare `Runner.Env` keys without exposing them in the
container-runtime argv, while `Runner.OmitEnv` removes inherited backend
credentials that a caller replaces. Rootless podman gets a `harness-proxy`
sidecar on that network, restricted to `Harness.EgressHosts()` and any extra
hosts in `Runner.Sidecar`; Docker Desktop uses the same sidecar because an
internal network cannot reach its host proxy. Other runtimes use
`Runner.ProxyURL` through the network gateway. Runner images used for this
path need curl and the `cmd/harness-proxy` binary. Call `SweepHardened` during
startup to remove networks and sidecars left by an interrupted process.

`egress` contains the authenticated allowlist proxy and
`WriteSandboxSettings`, which writes Claude's `.claude/settings.json` domain
allowlist.

`llm` sends a single schema-constrained request to the Anthropic Messages API.
It accepts a caller-owned HTTP client and permits plain HTTP only for local
development endpoints.

## License

MIT
