# cpex plugin

> **`cortex-cpex` is a build variant** of the AuthBridge proxy-sidecar
> (`cortex`), deployed *in place of* `cortex` when
> CPEX policy enforcement is needed — not an additional sidecar.

The `cpex` plugin embeds the [CPEX](https://github.com/contextforge-org/cpex)
runtime inside AuthBridge so operators can drive policy with CPEX's
APL (Attribute Policy Language) DSL — and any of the pre-built CPEX
sub-plugins (Cedar PDP, PII scanner, audit logger, JWT identity,
OAuth delegation) — without writing Go.

A single AuthBridge plugin instance (`cpex`) wraps an entire CPEX
`PluginManager`. Operators declare which CPEX hook names fire on each
AuthBridge phase via the `hooks` block; CPEX's own YAML defines the
sub-plugins those hooks dispatch to.

```text
┌──────────────────────────────────────────────────────────────────┐
│ cortex-cpex (binary built with -tags cpex, links libcpex_ffi)│
│                                                                  │
│  pipeline:                                                       │
│   ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐ │
│   │ jwt-validation│ │ mcp-parser  │→│ cpex (this plugin)      │ │
│   └─────────────┘  └─────────────┘  │                         │ │
│                                     │  on_request:            │ │
│                                     │    cmf.tool_pre_invoke ─┼─┼──► CPEX FFI
│                                     │  on_response:           │ │
│                                     │    cmf.tool_post_invoke ┼─┼──► CPEX FFI
│                                     └─────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
                                                       │
                                                       ▼
                                       ┌──────────────────────────┐
                                       │ CPEX runtime (in-process │
                                       │ via cgo + libcpex_ffi.a) │
                                       │                          │
                                       │  ┌─────────┐ ┌─────────┐ │
                                       │  │  APL    │ │ Cedar   │ │
                                       │  │  rules  │ │  PDP    │ │
                                       │  └─────────┘ └─────────┘ │
                                       │  ┌─────────┐ ┌─────────┐ │
                                       │  │ PII scan│ │ Audit   │ │
                                       │  └─────────┘ │ logger  │ │
                                       │              └─────────┘ │
                                       └──────────────────────────┘
```

## When to use

The cpex plugin is the right home for policy that:

- **Combines multiple decision shapes**. APL's flat predicates
  (`require(role.hr)`) compose naturally with Cedar's principal-×-resource
  rules and IdP-side delegation; expressing that in a single AuthBridge
  Go plugin would mean reimplementing each engine.
- **Needs to be edited by non-developers**. The APL DSL + Cedar
  policies live in operator YAML; updates ship via ConfigMap reload
  rather than a plugin re-deploy.
- **Composes pre-built CPEX sub-plugins** — PII scanners, audit
  loggers, OAuth delegators — instead of writing those from scratch in
  Go.

It is **not** the right home for:

- Plugins whose only job is one stable Go decision (`jwt-validation`,
  `token-exchange`) — those stay as AuthBridge-native plugins.
- Policy that needs to run *before* any parser has classified the
  body. cpex requires at least one of `mcp-parser` /
  `inference-parser` / `a2a-parser` upstream in the chain.

## Architecture

### Build constraint

The cpex plugin links `libcpex_ffi.a` via cgo. The `cortex-cpex`
binary (`cmd/cortex-cpex/`) is the only build target that compiles
the plugin — built with `-tags cpex` and `CGO_ENABLED=1`. Other
binaries (`cortex`, `cortex-envoy`, `authbridge-lite`)
stay pure-Go and never import the plugin.

The plugin's chassis (config decode, dispatch, Invocation recording)
is tag-free; only the cgo adapter that talks to `cpex.PluginManager`
is `//go:build cpex`. Unit tests run with `CGO_ENABLED=0` against a
fake Manager.

### Hook chains

Each AuthBridge phase (`OnRequest`, `OnResponse`) dispatches an
ordered list of CPEX hook names. Hooks fire one at a time; the chain
short-circuits on the first sub-plugin that returns deny. A sub-plugin
that returns `modify` records the Invocation, applies its modifications
to pctx (rewritten body, mutated headers, session labels), and lets the
chain continue. CPEX's standard hook names:

| Hook | Fires for | Typical sub-plugins |
|---|---|---|
| `cmf.tool_pre_invoke` | MCP tool call about to be forwarded | Cedar PDP, PII scanner, validators |
| `cmf.tool_post_invoke` | MCP tool result returned | Audit logger, post-call delegators |
| `cmf.llm_input` | LLM inference request | Prompt-PII redactors |
| `cmf.llm_output` | LLM inference response | Output redactors, audit |

### Decision vocabulary

CPEX sub-plugin verdicts map onto AuthBridge's 5-value
`InvocationAction`:

| CPEX | AuthBridge | Chain effect |
|---|---|---|
| Allow | `allow` | Continue to next hook |
| Deny  | `deny`  | **Stop chain**, return `pipeline.Reject` |
| Modify | `modify` | Continue to next hook (mutations applied to pctx) |
| Observe | `observe` | Continue (diagnostics, no flow change) |

A CPEX-internal failure (FFI panic, runtime error) is distinct from a
policy `deny` — it surfaces as `cpex.error` and respects the
`fail_open` flag (see Configuration).

## Configuration

A `cpex` entry carries two kinds of configuration, and they live at
different levels:

- **AuthBridge-side fields** are the keys of the plugin's `config:`
  block: which CPEX hooks fire on each phase, what happens when CPEX
  itself fails, and which traffic skips CPEX. The table below is the
  complete list. Any other key is rejected at boot with an
  `unknown field` error.
- **The CPEX runtime YAML** declares the sub-plugins, PDPs and routes
  CPEX loads. Cortex does not parse or reshape it. It arrives as a file
  (`config_file`) or as one string (`config`) and goes to CPEX's
  `LoadConfig` verbatim, so its schema is CPEX's own: see
  [CPEX's configuration reference](https://contextforge-org.github.io/cpex/docs/configuration/).

```yaml
plugins:
  - name: cpex
    config:
      hooks:
        on_request:
          - cmf.tool_pre_invoke
        on_response:
          - cmf.tool_post_invoke
      # The CPEX runtime YAML, mounted from a ConfigMap.
      config_file: /etc/cpex/cpex.yaml
      fail_open: false
      worker_threads: 2
```

| Field | Type | Default | Purpose |
|---|---|---|---|
| `hooks.on_request` | `[]string` | empty | CPEX hook names fired during OnRequest, in order. Empty makes the phase a no-op. |
| `hooks.on_response` | `[]string` | empty | CPEX hook names fired during OnResponse, in order. |
| `config_file` | string | unset | Path to the CPEX runtime YAML. Read when the plugin is configured; Cortex does not watch it for changes. Mutually exclusive with `config`. |
| `config` | string | unset | The CPEX runtime YAML inline, as a single string. Mutually exclusive with `config_file`. |
| `fail_open` | bool | `false` | On a CPEX-internal error: reject (`false`) or log and continue (`true`). A policy deny is always honored. See [fail_open](#fail_open-when-to-use-it). |
| `worker_threads` | int | `0` | Size of CPEX's tokio worker pool; `0` lets CPEX choose. |
| `bypass_hosts` | `[]string` | Keycloak, SPIRE, observability | Host globs that skip CPEX, on outbound traffic only. Setting the list replaces the default rather than extending it. See [Bypass list curation](#bypass-list-curation). |
| `bypass_paths` | `[]string` | `/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*` | URL path globs that skip CPEX in both directions. Setting the list replaces the default. |

A `cpex` entry with no hooks and no CPEX YAML is valid and inert, so the
plugin can be installed before its policy ships.

### The CPEX runtime YAML

The top-level sections this example uses are `plugins:` (the sub-plugins
policy can call), `global:` (cross-cutting PDPs such as Cedar, and the
session store), `routes:` (the APL policy, one entry per tool, prompt or
resource) and `plugin_settings:`. It is trimmed from
[`demos/hr-cpex/k8s/cpex-policy.yaml`](../demos/hr-cpex/k8s/cpex-policy.yaml),
the complete working example:

```yaml
plugin_settings:
  routing_enabled: true

plugins:
  - name: jwt-user
    kind: identity/jwt
    hooks: [identity.resolve]
    on_error: fail
    config:
      role: user
      header: X-User-Token
      trusted_issuers:
        - issuer: "https://keycloak.example.com/realms/prod"
          audiences: ["cpex-gateway"]
          algorithms: ["RS256"]
          decoding_key:
            kind: jwks_url
            url: "https://keycloak.example.com/realms/prod/protocol/openid-connect/certs"
      claim_mapper: standard

  - name: pii-scan
    kind: validator/pii-scan
    hooks: [cmf.tool_pre_invoke]
    on_error: fail
    config:
      detect:
        - { kind: ssn }
        - { kind: credit_card }
      mode: deny

global:
  apl:
    pdp:
      - kind: cedar-direct
        dialect: cedar
        policy_text: |
          @id("engineering-internal-repos")
          permit(
            principal,
            action == Action::"read",
            resource is Repo
          ) when {
            principal.roles.contains("engineer") &&
            resource.visibility == "internal"
          };

routes:
  - tool: search_repos
    authentication:
      - jwt-user
    authorization:
      pre_invocation:
        - "require(team.engineering)"
        - cedar:
            action: 'Action::"read"'
            resource:
              type: Repo
              id: ${args.repo_name}
              attributes:
                visibility: ${args.visibility}

  - tool: send_email
    authentication:
      - jwt-user
    authorization:
      pre_invocation:
        - "require(perm.email_send)"
        - "run(pii-scan)"
```

To keep everything in one place, the same YAML can go inline under
`config:` instead. It is a string, so it needs a block scalar (`|`):

```yaml
plugins:
  - name: cpex
    config:
      hooks:
        on_request:
          - cmf.tool_pre_invoke
      config: |
        plugin_settings:
          routing_enabled: true
        plugins:
          - name: pii-scan
            kind: validator/pii-scan
            hooks: [cmf.tool_pre_invoke]
            on_error: fail
            config:
              detect:
                - { kind: ssn }
              mode: deny
        routes:
          - tool: send_email
            authorization:
              pre_invocation:
                - "run(pii-scan)"
```

### Pipeline composition

cpex declares `RequiresAny: [mcp-parser, inference-parser, a2a-parser]`.
At least one must appear earlier in the chain so the parser has populated
`pctx.Extensions.MCP` / `.Inference` / `.A2A` before cpex extracts CMF
content. `Pipeline.Build` rejects misordered chains at boot.

cpex also declares `ReadsBody`, `WritesRequestBody` and `WritesResponseBody`.
Request-body writers chain, so cpex can share a chain with another request
mutator (`tool-prune`, say), each seeing the body as the one before it left it.
A second *response* mutator (`sparc`, say) still fails at boot: at most one
plugin per pipeline rewrites responses.

A typical inbound chain:

```yaml
plugins:
  - name: jwt-validation       # identity gate
  - name: mcp-parser           # populate pctx.Extensions.MCP
  - name: cpex                 # APL/Cedar/PII via CPEX
```

## Status codes & reasons

| Code | HTTP (non-MCP) | Meaning |
|---|---|---|
| `cpex.error` | 502 | CPEX FFI returned an error and `fail_open: false`. Body carries the underlying error message. |
| `cpex.denied` | 403 | A CPEX sub-plugin returned deny without a violation code. |
| `cpex.<sanitized>` | 403 | A CPEX sub-plugin returned deny with a violation code; the `<sanitized>` part lower-cases letters, replaces other characters with underscores, e.g. `pii.detected` → `cpex.pii_detected`. |

The `Invocation.Reason` field carries the CPEX-side reason string for
all of the above so operator dashboards can render the original
sub-plugin message alongside the namespaced code.

**HTTP status / wire shape.** The plugin sets a status on every
rejection — **403 Forbidden** for a policy `deny`, **502 Bad Gateway**
for a CPEX-internal error (the policy engine itself failed — an upstream
fault, not a client error). It must set these explicitly: the namespaced
`cpex.*` codes are dynamic and never appear in the listener's static
`codeToStatus` table, so without an explicit status `Violation.Render`
would default every CPEX outcome to 500.

How that reaches the client depends on the listener and the request. On
the HTTP proxies (forward / reverse), a **classified MCP request** (the
MCP extension is populated with a JSON-RPC `method` and `id`) is rendered
as an **MCP JSON-RPC 2.0 error frame at HTTP 200**, with the id echoed
back:
`{"jsonrpc":"2.0","id":<id>,"error":{"code":-32000,"message":<reason>,"data":{"error":"cpex.<code>","plugin":"cpex"}}}`.
The caller's MCP client then surfaces a failed tool call rather than a
transport break, and the `cpex.*` code rides in `error.data.error` (not
the HTTP status). Non-MCP requests — and JSON-RPC notifications with no
id — get the plain 403 / 502 in the table above. (See
`core/listener/httpx/render.go`; the gRPC listener,
extproc, doesn't use this renderer.)

## Bypass list curation

`bypass_hosts` and `bypass_paths` short-circuit *before* the FFI call,
so traffic to infrastructure that shouldn't see CPEX policy (Keycloak,
SPIRE, observability) skips the cgo round-trip entirely.

Both lists are operator-extensible glob patterns (`path.Match` syntax).
Default `bypass_hosts` covers `keycloak.*`, `spire-server.*`,
`spire-agent.*`, `otel-collector.*`, `jaeger.*`, `prometheus.*`,
matching the IBAC plugin's default set. Default `bypass_paths` covers
`/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*` via
`core/bypass.DefaultPatterns` — the same set jwt-validation uses.

Patterns that match everything (`"*"`, `""`, `"/*"`) are rejected at
Configure time; that gesture is better expressed by removing the cpex
plugin from the pipeline.

## fail_open: when to use it

`fail_open: false` (default) treats a CPEX-internal failure as a
deny. This is the right choice when CPEX is the only enforcement layer
for the traffic class — silently allowing requests when the policy
engine is broken is rarely what operators want.

`fail_open: true` allows requests through when CPEX errors, logging
the failure. Use this only when:

- Another enforcement layer (Envoy ext_authz, an upstream gateway)
  will gate the same traffic, and cpex is observation-only; OR
- The cost of brief unavailability outweighs the cost of brief
  unenforced traffic during a known-recoverable CPEX issue.

The `Invocation.Reason` field captures the underlying error in both
modes so the failure is auditable.

## Custom extension passthrough

Operators upstream of cpex can stash policy-input blobs in
`pctx.Extensions.Custom` under the `cpex/` prefix; cpex forwards them
into the CPEX `Extensions.Custom` map with the `cpex/` prefix
stripped. CPEX sub-plugins read them at the bare key.

```go
// In an upstream plugin's OnRequest:
pctx.Extensions.Custom["cpex/tenant_tier"] = "enterprise"

// On the CPEX side, an APL predicate sees it as:
//   custom.tenant_tier == "enterprise"
```

Entries without the `cpex/` prefix stay private to the plugin that
set them (rate-limiter cookies, plugin-internal state) and never
cross the FFI boundary.

## Sensitive headers

A fixed allowlist of sensitive header prefixes and exact names is
stripped from the CMF payload before crossing the FFI boundary —
CPEX sub-plugins (notably `audit/logger`) frequently log the payload
they receive, and the AuthBridge session API has no auth on it.

Stripped prefixes: `authorization`, `cookie`, `set-cookie`,
`proxy-authorization`, `x-amz-security-token`.

Stripped exact names: `x-api-key`, `x-auth-token`, `x-authorization`,
`x-secret-token`, `x-session-token`, `x-csrf-token`,
`x-platform-secret`, `x-authbridge-secret`.

This list is conservative; adding a header here is safer than
discovering one in an audit log.

## Background tasks

CPEX sub-plugins that emit asynchronous work (the canonical case is
`audit/logger` writing to an external sink) return their results
through `BackgroundTasks`. The cpex plugin spawns a goroutine per
Invoke that awaits the background work and routes per-sub-plugin
errors through the default `slog` handler with the request ID.

Operators pipe `slog` output to their observability stack. There is no
configurable audit sink today; future work may add one if operators
need to route background results separately from foreground logs.

## Limitations

- **Single-value body redaction; multi-part rewrites fail closed.** Body
  modifications **are** re-serialized. When a CPEX sub-plugin returns
  `modify` with a body payload change (the PII scanner / `redact` path is
  the canonical case), the cpex plugin rewrites `pctx.Body` format-aware
  per protocol — MCP `tools/call` arguments and results, inference
  messages, and A2A artifact parts (see `applyBodyModFromCMF` and the
  per-format `applyMCP*/applyInference*/applyA2A*` write-backs). The
  residual limitation is value cardinality: the CMF write-back carries a
  single redacted value, so a body with more than one redactable text
  part (an MCP result with multiple text blocks, a multi-part A2A
  artifact) is ambiguous to rewrite and is **rejected fail-closed** rather
  than partially redacted. Single-part bodies — the common case, and what
  the demo exercises — redact and forward normally.
- **Per-sub-plugin Invocations** require the CPEX FFI to expose
  per-sub-plugin outcomes; today CPEX returns an aggregate
  `PipelineResult` with a single `Violation`. The cpex plugin emits
  one aggregate Invocation per Invoke as a result. A CPEX-side
  change to expose `PipelineResult.SubPlugins[]` will let the
  AuthBridge plugin emit one Invocation per sub-plugin.
- **A2A traffic** is not auto-classified into a CPEX hook. Operators
  configure a chain explicitly via `hooks.on_request: [<some hook>]`
  even for A2A; CPEX's own routing handles per-sub-plugin filtering.

## Failure modes (detailed)

| Symptom | Cause | Fix |
|---|---|---|
| Pipeline build fails at boot: `cpex plugin: this binary was not built with -tags cpex` | Operator pointed `cortex` (no cgo) at a config containing a `cpex` plugin. | Use the `authbridge-cpex` image instead; or rebuild with `-tags cpex` against a downloaded `libcpex_ffi.a`. |
| Pipeline build fails: `cpex bypass_paths: invalid bypass pattern` | An entry in `bypass_paths` has bad `path.Match` syntax. | Fix the glob pattern (`/api/*/v1`, `/static/**` etc.). |
| Pipeline build fails: `cpex config: pattern "*" matches everything` | Operator wrote a wildcard pattern in `bypass_hosts` or `bypass_paths`. | If you want to disable cpex, remove it from the pipeline; don't bypass everything. |
| All traffic is rejected with `cpex.error` (a 200 MCP error frame for MCP calls, else 502), JWKS errors in logs | A CPEX `identity/jwt` plugin can't fetch its JWKS from Keycloak. | Check that the plugin's `decoding_key.url` in the CPEX YAML is reachable from inside the pod, and that `bypass_hosts` still covers Keycloak (the default does). |
| Audit logger silently produces no records | `BackgroundTasks` goroutine started but the audit sink is unreachable. | Search the slog stream for `cpex: background sub-plugin error` — the request ID and elapsed time correlate the failure with the foreground request. |

## See also

- [`core/plugins/cpex/README.md`](../core/plugins/cpex/README.md) — the
  plugin's own reference: CMF mapping, APL examples, building and testing.
- [`cmd/README.md`](../cmd/README.md) — the `cortex-cpex` binary and the
  `authbridge-cpex` image it ships in.
- `cmd/cortex-cpex/CPEX_FFI_VERSION` — pinned CPEX FFI ABI version
  the binary was built against.
- [CPEX documentation](https://contextforge-org.github.io/cpex/docs/) —
  the runtime YAML, APL, and the built-in sub-plugins.
- [CPEX repository](https://github.com/contextforge-org/cpex) — source
  and the FFI ABI.
- [`demos/hr-cpex/`](../demos/hr-cpex/) — runnable demo configurations.
