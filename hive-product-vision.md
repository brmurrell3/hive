# Hive: Product Vision & Roadmap

**One sentence:** Hive lets teams define AI agent pipelines in simple config files, run them in secure sandboxes, and deploy them anywhere -- from a laptop to an air-gapped government data center.

---

## The Problem

Companies are spending billions on AI agents, but the tooling is broken in three specific ways.

**Problem 1: Security is bolted on, not built in.**
When an AI agent runs code -- reviewing a pull request, executing tests, scanning for vulnerabilities -- it needs to be isolated so it can't accidentally (or maliciously) access things it shouldn't. Today, teams have to buy an orchestration tool (LangGraph, CrewAI) AND a separate sandbox tool (E2B, Fly.io) and glue them together. This is fragile, expensive, and creates security gaps at the seams. AWS offers both in one package (Bedrock AgentCore), but only if you're locked into AWS.

**Problem 2: Agent workflows aren't version-controlled.**
Software teams solved deployment reliability years ago with CI/CD -- build pipelines defined in config files, committed to the repository, triggered automatically. Agent workflows haven't caught up. They're written in Python scripts, configured through web UIs, and impossible to review, reproduce, or roll back. GitHub just launched "Agentic Workflows" (February 2026) but it only supports one agent at a time, not coordinated teams.

**Problem 3: You can't run agents where you need to.**
Cloud-only platforms can't serve the Pentagon (which explicitly requires AI tools on air-gapped networks), hospitals (HIPAA demands data stays on-premise), or banks (regulators require full audit trails). These are the highest-paying customers in the market, and nobody is serving them well. Python-based frameworks make this worse -- try running `pip install` on a disconnected network.

---

## The Solution

Hive is an open-source framework where:

- **Agents are defined in YAML files** -- simple config, not code. A three-agent pipeline is three files.
- **Each agent runs in its own secure sandbox** -- Firecracker microVMs (the same technology AWS Lambda uses) give each agent its own isolated kernel, memory, and network. One agent can't see or affect another.
- **Everything runs from a single binary** -- no Docker, no Kubernetes, no Python dependencies. One Go executable with an embedded message bus. Works on a laptop, a rack server, or an air-gapped machine.
- **The same config works everywhere** -- develop on your Mac, test on Linux, deploy to Hive Cloud or your own infrastructure. Zero config changes.

---

## Why Hive Wins

| | LangGraph / CrewAI | E2B | AWS AgentCore | Hive |
|---|---|---|---|---|
| Multi-agent orchestration | Yes | No | Yes | Yes |
| Per-agent VM isolation | No | Yes (sandbox only) | Yes | Yes |
| Single integrated platform | Orchestration only | Sandbox only | Yes, but AWS-locked | Yes, vendor-neutral |
| Self-hosted / air-gapped | Difficult (Python deps) | No | No | Yes (single Go binary) |
| Declarative config files | Python code | API calls | Console + SDK | YAML in your repo |
| Open source | Partially | Partially | No | Fully (Apache 2.0) |

**The key insight:** Nobody else ships orchestration + isolation + declarative config + self-hosted deployment in one package. Hive is the only framework where you define agent teams in config files and each agent runs in its own microVM, and it works on-premise.

---

## Who Buys This

**Tier 1 -- Developer teams (free, open source)**
Engineering teams that want to add AI agent pipelines to their CI/CD. They define agent teams in YAML, run them locally, and eventually deploy to Hive Cloud. This is the acquisition funnel.

**Tier 2 -- Mid-market companies (Hive Cloud)**
Companies with 10-100 engineers that want managed agent infrastructure. They don't want to run Firecracker fleets or manage NATS clusters. They push config files and Hive handles the rest.

**Tier 3 -- Regulated enterprises (Enterprise license)**
Defense contractors, hospitals, banks, and government agencies that need self-hosted deployment with compliance certifications. This is the highest-margin segment. They pay annual contracts for the enterprise package, air-gapped deployment support, and dedicated engineering.

---

## Revenue Model

| Tier | What They Pay | Price Point | Target |
|---|---|---|---|
| Free (open source) | Nothing. Self-hosted. Community support. | $0 | Individual developers, evaluation |
| Hive Cloud -- Team | Per agent-minute. First 1,000 minutes free/month. | ~$0.10/agent-minute | Startups, mid-market teams |
| Hive Cloud -- Business | Volume pricing. Priority support. SSO/RBAC. | ~$500-2,000/month | Growing engineering orgs |
| Enterprise Self-Hosted | Annual license. Compliance certs. Dedicated support. | $50,000-200,000/year | Defense, healthcare, finance |

**Why this works:** The open-source framework is the top of the funnel. Developers try Hive locally, build agent teams, and hit a natural boundary: they want to run in production without managing infrastructure. That's when they upgrade to Hive Cloud. Enterprise customers skip the funnel entirely -- they need self-hosted, air-gapped deployment with compliance certifications, and they'll pay six figures for it.

**Comparable revenue in the space:**
- LangChain/LangSmith: ~$16M ARR on $260M funding (observability SaaS)
- deepset/Haystack: ~$15M ARR on $46M funding (enterprise platform + on-prem)
- CrewAI: ~$3.2M ARR on $24M funding (execution-based SaaS)
- Dify: ~$3.1M ARR on $41M funding (cloud + self-hosted)

The deepset model is the best comparable for Hive: enterprise platform + on-premise deployment for regulated industries, built on an open-source framework.

---

## Roadmap: Four Phases

### Phase 1: The Demo (Weeks 1-4)

**What it is:** A pre-built agent team that ships with Hive. Clone the repo, run one command, watch three AI agents collaborate to review code, run tests, and scan for security issues. Runs on any laptop. No cloud account needed.

**Why it matters:** This is the "aha moment" that gets developers to star the repo, share it on social media, and try it for their own projects. Without a compelling demo, nothing else matters.

**What success looks like:** 500+ GitHub stars in the first month. Developers can go from `git clone` to a working demo in under 5 minutes.

### Phase 2: The Developer Experience (Weeks 3-8)

**What it is:** The tools developers need to build their own custom agent teams. Template library (CI/CD pipeline, research team, content pipeline, data processing), hot-reload development mode, runtime integrations so existing OpenClaw users can bring their agents into Hive, and SDKs for Python/Go/TypeScript.

**Why it matters:** Phase 1 gets attention. Phase 2 converts attention into adoption. Developers need to go from "cool demo" to "I'm using this for my project" with minimal friction.

**What success looks like:** 10+ external contributors. Developers building custom agent teams and sharing them publicly.

### Phase 3: Production Security (Weeks 6-12)

**What it is:** The full Firecracker integration working end-to-end. Pre-built VM images downloadable from GitHub (no manual building). Per-agent network policies (this agent can't access the internet, that agent can only reach specific domains). Resource limits enforced at the hardware level (an agent can't consume more memory than allocated). Shared directories between agents on the same team.

**Why it matters:** This is Hive's core differentiator. It's the feature that no other open-source framework has. It's also the prerequisite for selling to regulated enterprises -- they need to see real VM isolation, not just process-level separation.

**What success looks like:** 3+ organizations running Firecracker-isolated agents in staging or production. A published security assessment validating the isolation model.

### Phase 4: Hive Cloud (Weeks 10-18)

**What it is:** The managed commercial service. Customers push their YAML config files (via CLI or GitHub integration) and Hive Cloud handles everything: provisioning VMs, managing the message bus, autoscaling based on demand, scaling to zero when idle. Plus an enterprise tier for self-hosted deployment with compliance certifications.

**Why it matters:** This is the revenue engine. The open-source framework proves the technology. Hive Cloud removes the operational burden that prevents most teams from running agent infrastructure in production.

**What success looks like:** 10+ paying customers within 90 days. $100K ARR within 6 months. 3+ regulated-industry prospects in evaluation for enterprise contracts.

---

## Market Timing

The agentic AI market was ~$7.8B in 2025 and is growing at ~45% annually toward $50-100B by 2030. Only 2% of enterprises have deployed agents at scale, but 99% plan to. The gap between intent and execution is where infrastructure companies make money.

The window for a new entrant is roughly 12-18 months. AWS, Google, and Microsoft are rapidly expanding their agent services, and consolidation is accelerating (782 AI acquisitions in 2025). But cloud providers can't serve air-gapped deployments, and their platforms are locked to their ecosystem. Hive's vendor-neutral, self-hostable positioning is structurally defensible against cloud provider commoditization.

Standards are still forming (MCP, A2A, AG-UI all emerged in 2025). Early movers who establish the declarative pipeline-as-code pattern -- the way GitHub Actions established CI/CD workflow definitions -- have an outsized advantage.

---

## What We're Not Building

Clarity on scope matters as much as the roadmap.

- **Not an agent framework.** Hive doesn't help you write agent logic, prompts, or tool integrations. Agents bring their own brains. Hive orchestrates, isolates, and routes between them.
- **Not an LLM proxy.** Agents call their own model endpoints. Hive doesn't sit in the inference path.
- **Not a visual builder.** The product is config files and CLI. A web UI may come later, but the core experience is text files in a git repository.
- **Not an edge/IoT platform (yet).** The architecture supports microcontrollers and Raspberry Pis, but we're deferring that until the market demands it. CI/CD and enterprise are the beachheads.
