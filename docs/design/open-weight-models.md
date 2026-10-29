# Open-weight models for Aries and Bearings

Status: **research and a proposal**, 28 October 2026 (ar-hcjyxibf).
Nothing in it is built, and nothing in it was run: every fact about a
provider, a server or a harness below was **read** in its documentation
or in reports about it, as of this date, and is marked unverified where
the sources disagreed. The slice that builds each part verifies it
against the release it ships with, as [dispatcher.md](dispatcher.md) §10
does for harnesses.

The question, from the maintainer: how could Aries use open-source and
open-weight models when they are available, by API or locally, beside
Claude, Gemini, OpenAI and Cursor? The research on those providers' own
CLIs (Codex, Gemini CLI, Cursor's CLI), their headless runs and the
metadata their sessions record is a sibling issue, ar-uhs4k7ja; this
note links to it where a harness's details matter and does not repeat
it.

It covers how to reach the models (§1), which harnesses can drive them
(§2), how a project would say which model runs which work (§3), who
holds the credentials (§4), usage and cost (§5), what leaves the machine
and what the licenses say (§6), and ends with a recommendation (§7) and
the slices filed for it (§8).

## 1. Reaching open-weight models

There are three ways, and all three speak the same two wire formats:
OpenAI's Chat Completions (and, increasingly, its Responses API) and,
since early 2026, Anthropic's Messages API. That second format is what
makes this cheap for Aries: the one harness the dispatcher drives,
Claude Code, can be pointed at any endpoint that speaks it.

### 1.1 Hosted inference providers

Companies that serve open-weight models behind an API key, billed per
token: the inference clouds (Together, Fireworks, DeepInfra, Groq,
Cerebras and others), routers that front many of them under one key
(OpenRouter), and the model makers' own platforms (DeepSeek, Moonshot's
Kimi, Z.ai's GLM, MiniMax, Alibaba's Qwen). Every one offers an
OpenAI-compatible endpoint. Several of the model makers also offer an
**Anthropic-compatible endpoint** documented for Claude Code itself,
reached by setting `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` and
naming their model; the URL differs from the same provider's
OpenAI-compatible one.

What differs between them is not the API but the terms (§6): whether a
prompt is retained, for how long, and whether it may be trained on. A
router adds a second question: which provider behind it served the
request. OpenRouter lets a request require providers that do not
collect data (`provider.data_collection: "deny"`) or that keep none
(`provider.zdr: true`), enforced per request or for the account; its own
data processing agreement says the list reflects each provider's
published policy, not a guarantee of it.

### 1.2 Local serving

| Server | Fits | OpenAI-compatible | Anthropic Messages | Tool calls |
|---|---|---|---|---|
| Ollama | A developer's machine: one command pulls and runs a model | `/v1/chat/completions`, `/v1/responses` | `/v1/messages`, from v0.14.0; `ollama launch claude` writes Claude Code's settings | By the model's template; recommended for coding: Qwen3-Coder, GLM, MiniMax |
| llama.cpp (`llama-server`) | A developer's machine or a small server, with the most control over quantization and context | `/v1/chat/completions` | `/v1/messages`, merged January 2026, converted to the OpenAI path inside | Needs `--jinja` and a chat template that supports tools |
| vLLM (and SGLang) | A shared GPU server: batching, many sessions at once | `/v1/chat/completions`, `/v1/responses` | An Anthropic Messages handler, converted to the OpenAI path inside | Needs the tool-call parser that matches the model's output (`qwen3_coder`, `hermes` and others); the wrong one reads no tool call at all |
| LM Studio | A person's desktop | Yes | Not read | By template |

Two facts shape the rest of this note:

- **A local endpoint has no real credential.** Ollama ignores the token
  it is sent. A shared vLLM server needs one in front of it (a reverse
  proxy with a key or mTLS), which the server's operator owns, not
  Aries.
- **Context and memory decide the model, not the server.** Community
  guides for Claude Code against Ollama ask for at least 32k tokens of
  context and recommend 64k; Claude Code's own system prompt and tool
  schemas take a large part of that before the brief. A model that fits
  a laptop's memory at that context is a mid-sized one, quantized.

### 1.3 Through a gateway

A gateway (LiteLLM, or Bearings' own, [bearings.md](bearings.md) §3.24)
sits between the harness and any of the above, translates one format to
the other, holds the provider keys and meters each call. Bearings'
design already routes "self-hosted models behind OpenAI-compatible
endpoints the same way, cost from a configured rate per token or
GPU-hour" (§3.24); this note adds that the gateway should serve the
Anthropic format too, so that Claude Code reaches open-weight models
through it without a translation layer of the harness's own (§7).

## 2. Harnesses that can drive them

The harness, not the model, is what Aries meets: it runs the `prime`
hook, reports usage, calls the MCP tools, and obeys the guard. So the
question is which harness runs an open-weight model headless with tools,
and keeps those four.

| Harness | Reaches open-weight models by | Headless | `prime` hook, usage, guard | Notes |
|---|---|---|---|---|
| **Claude Code** (`claude -p`) | `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` to any Anthropic-compatible endpoint: Ollama, llama.cpp, vLLM, a provider's endpoint or a gateway; `--model` names the endpoint's model | Yes, today's dispatcher | **All three, unchanged**: the hooks, the transcripts `internal/capture` reads, the PreToolUse guard | The only harness Aries drives today. Built for Claude: its prompts and tool schemas are tuned to Claude's tool use, and other models follow them less well (§2.1) |
| Codex CLI (`codex exec`) | `codex --oss` (Ollama or LM Studio, found on the machine), or `[model_providers.NAME]` with `base_url` and `env_key` | Yes | ar-uhs4k7ja: usage and hooks | Read: Chat Completions support is deprecated or removed, so a provider must speak the Responses API (Ollama and vLLM do) |
| Gemini CLI, and Qwen Code (its fork) | Qwen Code takes `OPENAI_BASE_URL`, `OPENAI_API_KEY` and `OPENAI_MODEL`; Gemini CLI reaches Gemini only, or an OpenAI-compatible route through a gateway | Yes, `-p` with `--output-format json` or `stream-json` | ar-uhs4k7ja | Qwen Code is tuned to Qwen's coder models |
| OpenCode (`opencode run`) | Any OpenAI-compatible provider in `opencode.json` | Yes, `--format json` | No Aries hooks; usage from its JSON | Read: a provider's per-model options were dropped in headless runs in one release (1.14.48) |
| Cursor's CLI | Cursor's own models | Yes | ar-uhs4k7ja | Not a route to a model of one's own choosing |

### 2.1 What it costs in quality

Public agentic benchmarks put the strongest open-weight models of autumn
2026 (the GLM, Kimi, Qwen and DeepSeek families) close to the closed
frontier on terminal tasks, run in **each benchmark's own harness**.
Three things make that a ceiling, not a forecast, for this repository:

1. **The harness is Claude Code's, not the model's.** Its system prompt,
   tool set and edit format were written for Claude. Other models'
   providers document Claude Code as a supported client, which says it
   works, not how well.
2. **The model that runs locally is not the one benchmarked.** The
   leaders are hundreds of billions of parameters; what a developer's
   machine serves at 64k context is a smaller model or a hard
   quantization, and tool calls are what degrade first (community
   reports of failed file edits; wrong or missing tool-call parsers on
   vLLM).
3. **This workflow is long.** A run here reads `AGENTS.md` (about 60k
   characters), the issue and the brief, follows 18 workflow steps, runs
   the gate, opens a PR from a template and hands off with ticked items.
   Context length and instruction-following over a long session matter
   more than a benchmark's single task.

So the answer to "how well" is a measurement, not a reading: the
outcome record ([outcome-record.md](outcome-record.md)) already gives,
per issue, whether it landed, the rework and the CI attempts, and the
cost report gives tokens by model. A trial on low-risk issues (§8, the
third slice) compares a model by those, as ar-5dax5unw does for the
dispatcher itself.

## 3. Configuration: which model runs which work

Today a model label picks the run's model: `model:haiku`, `model:sonnet`
or `model:opus`, mapped in code to Claude's model ids
(`internal/dispatch/select.go`, `models` and `modelIDs`), and the run is
always `claude -p` against Anthropic. Three things are missing: a label
that names a model of another provider, the endpoint and credential that
reach it, and what to do when that endpoint is down.

### 3.1 Proposed: routes

A **route** is what a run needs to reach a model: a harness, an
endpoint, the model's id as the endpoint names it, and the name of the
credential. A model label names a list of routes, tried in order. Two
files, because a project decides which model does its work and a
machine decides what it can reach:

```yaml
# .aries/models.yaml, in the repository: the project's choice. No URL
# that is a machine's, and no secret.
labels:
  model:sonnet:
    - {harness: claude-code, endpoint: anthropic, model: claude-sonnet-5-5}
  model:open-coder:
    - {harness: claude-code, endpoint: local, model: qwen3-coder:30b}
    - {harness: claude-code, endpoint: hosted-open, model: example-coder-large}
  model:local-only:
    - {harness: claude-code, endpoint: local, model: qwen3-coder:30b}
```

```yaml
# models.yaml in aries's config directory: this machine's endpoints.
endpoints:
  anthropic: {format: anthropic, credential: anthropic}        # the provider's own URL
  local: {format: anthropic, url: "http://localhost:11434"}    # no credential
  hosted-open: {format: anthropic, url: "https://inference.example.com/anthropic", credential: hosted-open, leaves_machine: true}
```

- **No file, no change.** Without `.aries/models.yaml` the three labels
  of today keep their meaning, so a project that never uses another
  model sees nothing new.
- **Fallback is at the start, never mid-run.** At a wake, before the
  claim, the dispatcher probes each route's endpoint in order (a
  `HEAD` or the model list, with a short timeout) and starts on the
  first that answers. A run never switches model partway: its brief,
  its `brief:` record and its usage name one model. A label whose
  routes all fail is skipped with a rule of its own, as `no-model` is.
- **The record names what ran.** The `brief:` line already carries
  `model=` as a full id; with routes it also carries `endpoint=`, so an
  outcome can be read by route.
- **A label may forbid a route that leaves the machine.** `leaves_machine`
  on an endpoint, and a label (or an issue label `data:local`) that
  allows only endpoints without it, is the data rule of §6 as
  configuration.
- **Bearings takes the same file.** Its label affinity (bearings.md §3.9)
  and provider allowlist (principle 6) read the project's labels; the
  machine's endpoints become the gateway's routes, and its allowlist the
  policy's (§7).

The second harness for a route is ar-uhs4k7ja's to name; the schema
above has a `harness` field so that a route to Codex or Qwen Code needs
no new shape.

### 3.2 A hazard today

`Dispatcher.runEnv` (`internal/dispatch/dispatcher.go`) drops an
inherited `ANTHROPIC_API_KEY` and sets its own, but passes every other
`ANTHROPIC_*` variable through. A person whose shell sets
`ANTHROPIC_BASE_URL` (as every guide above tells them to, to try a local
model) starts dispatcher runs that send the brief, the issue and the
code to that endpoint, under a model the label did not name, with the
dispatcher's Anthropic key as the token. Filed as a bug (§8, the first
slice): the run's environment should drop `ANTHROPIC_BASE_URL`,
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL` and the `ANTHROPIC_DEFAULT_*`
model variables, and set them only from a route.

## 4. Credentials and accounts

| Kind | Example | Today | Under Bearings |
|---|---|---|---|
| A provider's API key | Anthropic, a hosted open-weight provider, a router | One key, `ANTHROPIC_API_KEY`, kept by `internal/credential` and read by `aries dispatch run` | In the gateway's secret store by reference, never in the agent's environment; the agent holds a session token bound to its lease (bearings.md §3.24) |
| A subscription login | A Claude plan, a provider's coding plan | Ruled out of unattended runs (Bearings decision 14); a person's own sessions only | Unchanged: it cannot pass through a gateway, and policy may forbid it in a scope |
| A local endpoint | Ollama on the developer's machine | Nothing to keep | Nothing to keep; the gateway routes to it and meters it |
| A shared self-hosted endpoint | vLLM on a team's GPU server, behind a proxy with a key | Would be one more named credential | The gateway's route, keyed in its store; the server's own access control is its operator's |

What each layer holds:

- **The dispatcher, until Bearings.** `internal/credential` already
  keeps secrets **by name** (`validName`; one service, many names). The
  dispatcher reads one. With routes, it reads the credential each route's
  endpoint names, at start, and puts it in the run's environment as
  `ANTHROPIC_AUTH_TOKEN` (or the harness's variable) and nowhere else:
  not argv, the brief, the state file, the log or Aries (rule 2).
  `aries dispatch key` gains a name. A hosted provider's key should
  carry a spend limit set in that provider's console, as the Anthropic
  key does (dispatcher.md §11), since Claude Code's `--max-budget-usd`
  prices by Claude's rates and is **unverified** for another provider's
  model.
- **Bearings.** Custody of every key (K0) and the gateway that stands
  between every harness and every provider (K1), as §3.24 has them. The
  routes of §3 become its routes; the open-weight endpoints are routes
  like any other, and "who holds the credential" stops being a question
  for the machine.
- **ariesd, never.** No provider key, token or endpoint credential is
  stored, sent or logged by the server (rule 2). What Aries holds about
  a model is its name on usage records and its price. Whether to mirror
  key metadata is Bearings' (§3.23, provider-key metadata in the Bearings
  schema, never values).

An endpoint URL is not a secret, but a machine's URL is the machine's:
it is in the machine's file, not the repository, and in this repository
never a real host (rule 1).

## 5. Usage and cost

**Capture works as it is for Claude Code routes.** `aries usage --hook`
reads Claude Code's transcripts (`internal/capture`), which record each
response's model and token counts as the endpoint returned them. Pointed
at Ollama, the record's model is the endpoint's name for it
(`qwen3-coder:30b`), which `proto.UsageModel` accepts (letters, digits
and `_.:/+@-`). Whether each server fills every usage field in its
Anthropic responses (cache reads and writes above all, which a local
server has no notion of) is **unverified**; a missing field is a zero,
not an error. Other harnesses' usage is ar-uhs4k7ja's (Q8: usage without
a hook).

**Prices.** Cost is the list-price equivalent of the tokens at each
model's rates, set by an admin (`aries admin price set`,
[docs/server.md](../server.md#prices)); a model with no price is shown
`unpriced`, never $0. For open-weight models:

- **A hosted model** is priced at its provider's per-token rates, under
  the model name the provider returns.
- **A local model** has three honest answers, and the admin picks:
  leave it **unpriced** (the report says its cost is a lower bound,
  and Sextant shows the tokens), price it at **zero** (the report then
  says it cost nothing, which is the marginal truth on a machine already
  paid for), or price it at a **computed rate**: a GPU-hour or a
  machine's monthly cost over the tokens it serves, which is what
  bearings.md §3.22 means by "self-hosted models still have a cost" and
  proposes a `unit` (`gpu_hour`) on prices for, as a 1.x addition.
- **The dispatcher's cap** (dispatcher.md §6) counts a day's Opus
  output tokens; tokens of another model do not count toward it, so a
  route to an open-weight model needs a cap of its own if it is billed.
  The provider's spend limit is the hard stop until Bearings' envelopes.

No protocol change is needed for any of this: usage records and prices
are keyed by a free model name already.

## 6. Data and licenses

### 6.1 What leaves the machine

A run sends the model everything the harness reads: the brief and the
issue's text, `AGENTS.md`, the files it opens, command output (test
logs, diffs), and the MCP tools' results, which include other issues,
comments and memories. On a hosted route all of it goes to that
provider; on a router, to whichever provider behind it served the
request.

- **Local routes** send nothing off the machine; that is their main
  reason to exist beside cost.
- **Hosted routes** are allowed per project, and the project chooses
  providers by their terms: no training on API data, and a stated
  retention, ideally none. The terms change and the sources disagreed
  for some providers as read here (§1.1); the slice that adds a route
  records the terms it read and their date.
- **A router** is configured to refuse providers that retain or collect
  data (OpenRouter's `data_collection: "deny"` or `zdr`), or not used.
- **The data rule as configuration**: an issue labeled `data:local`, or
  a label whose routes are all local, never runs on an endpoint marked
  `leaves_machine` (§3.1). This is the open-weight half of Bearings' Q13
  (may issue text go to third-party providers), which recommends the
  provider allowlist and the secrets lint first; the same answer holds
  here.

### 6.2 Model licenses

A model's license binds whoever **runs** it, so it matters for local and
self-hosted routes; on a hosted route the provider has taken the license
and its own terms bind the project. As read, by family (each checkpoint
has its own license, which the slice that adds a route reads, since a
family's later release may change it):

| Family | License as read | What to watch |
|---|---|---|
| Qwen (most text and coder models) | Apache-2.0 | Some sizes and older releases had their own license |
| DeepSeek (recent) | MIT | Earlier V3 releases had a model license of their own |
| OpenAI's gpt-oss | Apache-2.0, with a usage policy | — |
| GLM | MIT | — |
| Kimi (K2) | Modified MIT | Attribution in the product's interface past a user or revenue threshold |
| Mistral | Apache-2.0 for some, the Mistral Research License or commercial terms for others | Per model |
| Llama | Llama Community License | A monthly-active-user threshold, an acceptable-use policy, naming rules for derivatives |
| Gemma | Gemma Terms of Use | A prohibited-use policy that flows to derivatives |

Using a model to write code does not make the code a derivative of the
model under any of these as read; what a license restricts is use
(acceptable-use policies), scale (thresholds) and redistribution of the
weights or of fine-tunes. Aries redistributes no weights. A project's
model allowlist is where an organization records which licenses it
accepts.

## 7. Recommendation

1. **Reach open-weight models through Claude Code, not a new harness.**
   Every server worth using (Ollama, llama.cpp, vLLM) and several
   providers now speak Anthropic's Messages format, so a route is three
   environment variables and a `--model`, and the `prime` hook, usage
   capture and the guard keep working unchanged. Adding a second
   harness for open-weight models alone would rebuild all three; the
   second harness, when it comes, comes from ar-uhs4k7ja for its own
   providers, and the route schema already has a field for it.
2. **Fix the inherited endpoint now.** Dispatcher runs should not take
   `ANTHROPIC_BASE_URL` and its siblings from the person's shell (§3.2).
3. **Routes in the dispatcher, then in Bearings.** Labels map to routes
   (§3.1), with fallback at the start of a run, named credentials from
   `internal/credential`, and the `data:local` rule. Bearings' gateway
   (K1) takes the same file and serves the Anthropic format too, so the
   dispatcher's routes become its routes without a change to projects'
   files; §3.24's row for self-hosted models then covers local and
   hosted open-weight models alike.
4. **Measure before relying on one.** Run one local model on a handful
   of low-risk issues against the local dev server, and read the outcome
   records and the cost report beside Claude's. Until that trial says
   otherwise, an open-weight route is opt-in per label, never a fallback
   for a Claude label, so no issue silently runs on a weaker model.
5. **Price local models on purpose.** The guide for routes tells an
   admin to set a price for each route's model name: the provider's
   rates, zero, or a computed rate, and says that unpriced is a lower
   bound. Bearings' `unit` on prices waits for 1.x as §3.22 has it.
6. **The server holds nothing new.** No credential, endpoint or route in
   ariesd; no protocol change.

## 8. Slices

Filed as issues, each citing this note:

| Slice | Issue | Delivers | Needs |
|---|---|---|---|
| Inherited endpoint | ar-ffdzw3as | `runEnv` drops `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL` and the `ANTHROPIC_DEFAULT_*` variables; a test | — |
| Routes | ar-yxlvp53v | `.aries/models.yaml` and the machine's `models.yaml`; labels to routes with fallback at start; named credentials (`aries dispatch key NAME`); `endpoint=` in the `brief:` record; `data:local`; docs/agents.md's dispatcher section and the price guidance | This note merged |
| Trial | ar-2kx7c65e | One local model under Claude Code on three to five low-risk issues against the local dev server, read by outcome and cost beside Claude's; a comment with the result. The maintainer installs the serving software | Routes |

The Bearings side needs no issue of its own yet: K1's slice
(bearings.md §6) takes the routes file and the Anthropic format when it
is built, and this note is linked from §3.24.

## Sources

Read 28 October 2026. Vendors' pages unless marked.

- Ollama: [Anthropic compatibility](https://docs.ollama.com/api/anthropic-compatibility),
  [Claude Code with Anthropic API compatibility](https://registry.ollama.ai/blog/claude).
- llama.cpp: [New in llama.cpp: Anthropic Messages API](https://huggingface.co/blog/ggml-org/anthropic-messages-api-in-llamacpp).
- vLLM: [Anthropic serving](https://docs.vllm.ai/en/stable/api/vllm/entrypoints/anthropic/serving/),
  [the Qwen3 tool parser](https://docs.vllm.ai/en/stable/api/vllm/parser/qwen3/).
- Codex: [local configuration](https://developers.openai.com/codex/local-config);
  a guide to custom providers (third party):
  [codex.danielvaughan.com](https://codex.danielvaughan.com/2026/04/23/codex-cli-custom-model-providers-configuration-guide/).
- Qwen Code: [headless mode](https://qwenlm.github.io/qwen-code-docs/fr/users/features/headless/).
- OpenCode: a report of options dropped in headless runs,
  [anomalyco/opencode#27361](https://github.com/anomalyco/opencode/issues/27361).
- Providers' Anthropic-compatible endpoints for Claude Code:
  [MiniMax](https://platform.minimax.io/docs/coding-plan/claude-code);
  others from third-party guides, to be read at each provider when a
  route is added.
- OpenRouter: [zero data retention](https://openrouter.ai/docs/guides/privacy/zdr),
  [its post on ZDR](https://openrouter.ai/blog/insights/zero-data-retention/).
- Benchmarks (aggregators, third party, figures disagree by snapshot):
  [Terminal-Bench 2 on BenchLM](https://www.benchlm.ai/benchmarks/terminal-bench-2).
- Licenses (secondary; read each model's license file):
  [IntuitionLabs on open-weight licenses](https://intuitionlabs.ai/articles/open-weight-ai-model-licenses).
