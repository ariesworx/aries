# References and further reading

Aries is meant to support a *dark factory* for software, where coding
agents claim work, coordinate, ship and learn with few humans in the
loop. This page collects the articles, papers and specs behind that
idea. Blog posts and news pieces are tagged **users**. Papers and
specs are tagged **contributors**. Some work for **both**. Each note
says why the source matters for a specific part of Aries:

- `aries` is the client: a command line for people, and an MCP server
  for agents.
- `ariesd` is the coordination server: claims, leases, inbox, locks
  and events.
- `sextant` is observability of the agents' work: cost and telemetry
  export.
- `bearings` is the dark factory manager, the orchestrator planned on
  top of Aries (a design only; no code yet).
- The **learning loop** covers lessons, outcome records and
  experiments.

All links were checked on 26 Oct 2026.

## Suggested reading path (newcomers)

1. [Robots, the maker of robots, in FANUC's dark factory](https://www.organizedergi.com/News/5493/robots-the-maker-of-robots-in-fanuc-s-dark-factory):
   where the "lights-out" idea comes from.
2. [The Five Levels: from Spicy Autocomplete to the Dark Factory](https://www.danshapiro.com/blog/2026/01/the-five-levels-from-spicy-autocomplete-to-the-software-factory/):
   a vocabulary for how autonomous a software team is.
3. [The State of Context Engineering](https://www.dbreunig.com/2026/10/24/the-state-of-context-engineering.html):
   a current overview of how context engineering changed as models
   improved.
4. [Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents):
   the practical method: context as a finite attention budget.
5. [Welcome to Gas Town](https://yegge.ai/essays/welcome-to-gas-town/):
   what running 20 or more agents at once looks like, and why
   coordination becomes the bottleneck.

---

## Origins of the dark factory

- **[Robots, the maker of robots, in FANUC's dark factory](https://www.organizedergi.com/News/5493/robots-the-maker-of-robots-in-fanuc-s-dark-factory)**:
  Organize Dergi, 2023 · news · *users*
  How FANUC runs largely unattended production with more than 4,600
  robots. It also argues that the concept only pays off when the
  goals and budget are defined correctly. This is the manufacturing
  origin of the "dark factory" metaphor Aries borrows, and a reminder
  that autonomy is an investment you measure, not a vibe.

- **[The Five Levels: from Spicy Autocomplete to the Dark Factory](https://www.danshapiro.com/blog/2026/01/the-five-levels-from-spicy-autocomplete-to-the-software-factory/)**:
  Dan Shapiro, 2026 · blog · *users*
  A five-level ladder of AI-assisted development, from autocomplete
  up to the fully autonomous "dark factory". It is useful for placing
  Aries: `aries` and `ariesd` provide the plumbing for the upper
  levels, and `bearings` is the design for the top one.

- **[How StrongDM's AI team build serious software without even looking at the code](https://simonwillison.net/2026/Feb/7/software-factory/)**:
  Simon Willison, 2026 · blog · *both*
  A concrete report on a team running a "software factory" where
  agents write the code and humans don't review it line by line. It
  is a real-world benchmark for the workflows Aries coordinates, and
  shows why verification and outcome tracking (the learning loop)
  must replace human code review.

## Context and tokens

- **[Lost in the Middle: How Language Models Use Long Contexts](https://arxiv.org/abs/2307.03172)**:
  Nelson F. Liu et al. (Stanford et al.), 2023; TACL 2024 · paper ·
  *contributors*
  Shows that models use information at the start and end of a long
  context far better than information in the middle. This is the
  basic reason `aries` caps tool results at about 2,000 tokens and
  front-loads its prime/start briefings.

- **[Context Rot: How Increasing Input Tokens Impacts LLM Performance](https://www.trychroma.com/research/context-rot)**:
  Kelly Hong, Anton Troynikov, Jeff Huber (Chroma), 2025 · technical
  report · *both*
  Across 18 models, performance degrades unevenly as input length
  grows, even on simple tasks. It is evidence for keeping the `aries`
  MCP tool schema and inbox payloads small, rather than trusting
  large context windows.

- **[How Long Contexts Fail](https://www.dbreunig.com/2025/06/22/how-contexts-fail-and-how-to-fix-them.html)**:
  Drew Breunig, 2025 · blog · *users*
  Names four failure modes: context poisoning, distraction,
  confusion and clash. Use them as a checklist when designing
  `aries` briefings and the set of agent tools. Too many tools, for
  example, causes confusion.

- **[LLMs Get Lost In Multi-Turn Conversation](https://arxiv.org/abs/2505.06120)**:
  Philippe Laban, Hiroaki Hayashi, Yingbo Zhou, Jennifer Neville
  (Microsoft Research and Salesforce Research), 2025 · paper ·
  *contributors*
  When instructions arrive spread over several turns, models perform
  much worse and rarely recover from early wrong assumptions. This
  supports giving agents a complete, fresh briefing per claim
  (`aries prime`/`start`) instead of steering them through long
  sessions.

- **[Gemini 2.5: Pushing the Frontier with Advanced Reasoning, Multimodality, Long Context, and Next Generation Agentic Capabilities](https://arxiv.org/abs/2507.06261)**:
  Gemini Team, Google, 2025 · paper (technical report) ·
  *contributors*
  Its long-running agent case study (playing Pokémon) documents
  "context poisoning": hallucinated goals get stuck in context and
  derail later behavior. This is relevant to what `ariesd` should
  persist in the event log and inbox, and to when `bearings` should
  reset an agent.

## Context engineering

- **[Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)**:
  Anthropic, 2025 · blog · *both*
  Treats context as a finite "attention budget". It covers minimal
  tool sets, just-in-time retrieval, compaction, structured
  note-taking and sub-agents. Almost every principle maps onto an
  `aries` feature: capped tool results, briefings, lessons as notes,
  and dispatched sub-agents.

- **[Context Engineering for AI Agents: Lessons from Building Manus](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)**:
  Yichao "Peak" Ji (Manus), 2025 · blog · *contributors*
  Hard-won production advice: design around the KV cache, mask tools
  rather than removing them, use the file system as memory, and keep
  errors in context. It is directly useful for keeping the `aries`
  tool schema stable (cache-friendly) and for how the guard hook
  reports failures.

- **[The State of Context Engineering](https://www.dbreunig.com/2026/10/24/the-state-of-context-engineering.html)**:
  Drew Breunig, 2026 · blog (talk write-up) · *both*
  A current overview of how context engineering changed as models
  improved. Its framing, knowing when to be deterministic and when
  to let the model decide, matches the `bearings` principle "Go
  supervises; LLMs judge".

## Multi-agent coordination

- **[Welcome to Gas Town](https://yegge.ai/essays/welcome-to-gas-town/)**:
  Steve Yegge, 2026 · blog · *users*
  An essay on orchestrating 20–30 Claude Code instances at once. It
  pairs with Yegge's [Beads](https://github.com/gastownhall/beads), a
  distributed, graph-based issue tracker that serves as agent memory
  (the repo moved from `steveyegge/beads`). It is the closest peer
  project to Aries; compare its work tracking with `ariesd` claims
  and inbox, and its orchestration with `bearings`.

- **[How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)**:
  Martin Kleppmann, 2016 · blog · *contributors*
  The classic argument that a lease or lock is unsafe without fencing
  tokens, because processes pause and clocks drift. It is required
  reading before touching `ariesd` claims, leases and fencing epochs.

## Protocols and standards

- **[Model Context Protocol Specification (2026-07-28)](https://modelcontextprotocol.io/specification/2026-07-28)**:
  MCP project (Anthropic and contributors), 2026 · spec ·
  *contributors*
  The protocol `aries` speaks as an MCP server. The
  [release announcement](https://blog.modelcontextprotocol.io/posts/2026-07-28/)
  summarizes what changed. Track this version when changing the 24
  agent tools or their schemas.

- **[Agent2Agent (A2A) Protocol Specification](https://a2a-protocol.org/latest/specification/)**:
  The Linux Foundation (A2A project), 2026 (v1.0.0) · spec ·
  *contributors*
  An open standard for agents to discover each other, delegate tasks
  and stream results. It is a candidate interoperability layer if the
  `ariesd` inbox or the `bearings` wingman ever has to talk to agents
  outside Aries.

## Observability and cost

- **[Semantic conventions for GenAI agent and framework spans](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-agent-spans.md)**:
  OpenTelemetry, 2026 (Development status) · spec · *contributors*
  The emerging standard span and attribute names for agent
  invocations and tool calls. `sextant` exports over OTLP but
  deliberately does not emit `gen_ai.*` yet: a span carries ids,
  codes and small numbers and never text (design §16.4). Use this
  spec to decide which content-free attributes Aries could adopt so
  it works with Langfuse, LangSmith and similar backends.

## Evaluation

- **[SWE-bench: Can Language Models Resolve Real-World GitHub Issues?](https://arxiv.org/abs/2310.06770)**:
  Carlos E. Jimenez, John Yang, et al. (Princeton), 2023; ICLR 2024 ·
  paper · *both*
  The standard benchmark of 2,294 real GitHub issues that agents must
  resolve so the tests pass. OpenAI's human-validated subset,
  [Introducing SWE-bench Verified](https://openai.com/index/introducing-swe-bench-verified/)
  (2024), is the version most commonly reported. It is the natural
  yardstick for pre-registered learning-loop experiments.

- **[SWE Context Bench: A Benchmark for Context Learning in Coding](https://arxiv.org/html/2602.08316v2)**:
  Jiayuan Zhu, Junde Wu, Minhao Hu, et al., 2026 · paper ·
  *contributors*
  Built on SWE-bench. It measures whether agents benefit from reusing
  experience from related tasks. Well-selected summaries help, but
  poorly selected reuse can hurt. This is the closest published
  evaluation of Aries-style lessons and the outcome record, and a
  template for testing them.

## Learning loops

- **[Reflexion: Language Agents with Verbal Reinforcement Learning](https://arxiv.org/abs/2303.11366)**:
  Noah Shinn et al., 2023; NeurIPS 2023 · paper · *contributors*
  Agents improve across attempts by writing natural-language
  reflections on their failures into memory, with no weight updates.
  This is the academic basis for Aries lessons, which are recorded
  after an outcome and fed back into later briefings.

- **[Voyager: An Open-Ended Embodied Agent with Large Language Models](https://arxiv.org/abs/2305.16291)**:
  Guanzhi Wang et al. (NVIDIA, Caltech et al.), 2023 · paper ·
  *contributors*
  A lifelong-learning agent that builds a growing library of
  verified, reusable skills, with automatic curriculum design. It is
  a model for how the learning loop could grow from lessons into a
  curated, retrieval-indexed skill library, kept only when outcomes
  confirm the skill helps.
