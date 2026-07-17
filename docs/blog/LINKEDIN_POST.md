# LinkedIn post — TAG

> Paste-ready. Attach one of the new SVGs listed at the bottom after exporting it to PNG.

---

I spent months learning one thing the hard way:

**The agent isn't the product. The control plane around it is.**

Every agent framework nails the *engine* — the reasoning loop, tool calls, streaming. But the moment you run one for real, you hit the wall nobody talks about:

→ Which model should handle *this* task? Who verifies the output?
→ What did my agent do last week — and what did it cost?
→ How do I stop a runaway loop from burning $400 in tokens?
→ How do I give it memory that survives a restart?

None of that is the engine's job. So I built the missing layer.

Meet **TAG — Terminal Agent Gateway.**
A control-plane CLI that wraps an agent runtime and adds everything you need to actually *operate* agents:

**Memory that forgets on purpose** — every fact decays with a half-life based on its type (conventions never expire; ephemera fades in 60 days). BM25 search + a nightly GC that evicts noise.

**A knowledge graph for free** — mines entities from memory and clusters them with union-find. Zero LLM calls.

**Team-of-agents routing** — an orchestrator plans and delegates to cheap specialist models (researcher / coder / reviewer); expensive models only *verify*. Configure once in YAML.

**Budgets, alerts and HMAC webhooks** — cap tokens per profile, alert on cost/latency/pass-rate, turn a GitHub PR into an enqueued agent task.

**Everything recorded** — runs, costs, cache hits, OpenTelemetry GenAI traces. If it happened, you can see it and price it.

**103 commands. ~72 features. One-line install** (`pip install tag-agent`).

But the biggest lesson wasn't a feature — it was this:

**A green test suite is NOT proof.**
My tests hit the libraries directly and skipped the CLI dispatch path… so ~200 dispatch-layer bugs sailed straight through. The fix that actually worked: fan out parallel "auditor" agents, have each one *run the binary adversarially*, and independently verify every finding.

Read the code AND run it hostilely. It found bugs no test ever would.

Full build story — architecture, memory, hostile testing, and distribution. Link in comments.

What's *your* "green tests lied to me" story? I want to hear it.

#AIAgents #LLM #DeveloperTools #SoftwareEngineering #OpenSource #AgentOps #Observability #BuildInPublic

---

## First comment (drop the link here)
The full write-up, including the control-plane map, memory-decay chart, and hardening loop: [BLOG LINK]

---

## Hero-image suggestions (ready-made SVGs in `./assets/`)
Export any to PNG first — LinkedIn prefers PNG/JPG: `qlmanage -t -s 1600 -o . assets/<name>.svg`, or open the SVG in a browser and screenshot.

1. **`hero-operating-system.svg`** — best default. It communicates the runtime-vs-control-plane thesis in one image.
2. **`feature-surface.svg`** — best if the post hook is about scope: 103 commands, ~72 implemented capabilities, 127 PRDs.
3. **`hardening-taxonomy.svg`** — best if the post hook is "green tests lied to me."
4. **`runtime-vs-control-plane.svg`** — best for explaining how TAG differs from Hermes without pretending Hermes was not the engine.
5. **`memory-stack.svg`** — best for a technical audience interested in agent memory.

## Posting tips
- Put the blog link in the FIRST COMMENT, not the body (LinkedIn suppresses reach on posts with external links in the body).
- Post Tue–Thu, ~9–11am local.
- Keep the short paragraphs — LinkedIn rewards scannable text.
- End on the question — comments drive reach.
