# Fixclaw

Your team processes hundreds of emails a day. AI assistants like Claude Dispatch and OpenClaw handle personal inboxes. But they don't do compliance, audit trails, or defined business workflows.

Fixclaw does. It's a lightweight AI automation engine for business operations. Written in Go.

![Architecture](architecture.png)

<!-- TODO: Record a 15-second GIF of the Slack approval flow:
     email arrives > AI classifies > operator reviews in Slack > approved/rejected -->
<!-- ![Demo](demo.gif) -->

## Quickstart

```bash
git clone https://github.com/renezander030/fixclaw.git && cd fixclaw
cp secrets.yaml.example secrets.yaml   # add your Slack + API keys
go build -o fixclaw . && ./fixclaw
```

Define your pipelines in `config.yaml`, your prompts in `skills/`, and fixclaw handles the rest.

## Built for

Small businesses and ops teams that need AI automation with governance. Not for personal productivity. Use Dispatch for that.

Use cases:
- Route and classify incoming customer emails through defined pipelines
- Draft follow-ups with human approval before anything gets sent
- Summarize unread emails on a schedule with token budget enforcement

## Why not Dispatch or OpenClaw?

| | **Claude Dispatch** | **OpenClaw** | **fixclaw** |
|---|---|---|---|
| **Purpose** | Personal productivity | Personal AI agent | Business operations |
| **Governance** | Anthropic-managed | None | You own it: YAML pipelines, token budgets, audit trail |
| **Human-in-the-loop** | Pause on destructive actions | Optional | Every outbound action requires operator approval via Slack |
| **Token budgets** | None (subscription) | None | Per-step, per-pipeline, per-day limits |
| **Prompt injection defense** | Platform-level | None | Input sanitization + output schema validation built in |
| **Hosting** | Anthropic cloud | Self-hosted | Self-hosted. Your data stays on your infra |
| **Integrations** | Google Drive, Slack | Telegram, Signal, etc. | Microsoft 365, Gmail, Slack |
| **Configuration** | Natural language | Natural language | YAML. Deterministic, version-controlled, auditable |

Dispatch and OpenClaw are great for personal use. Fixclaw is what you deploy when the output touches customers, contracts, or compliance.

## How it works

Fixclaw runs pipelines. Each pipeline is a sequence of typed steps:

| Step type | What it does |
|---|---|
| `deterministic` | Plain code: fetch emails, filter, route, notify |
| `ai` | LLM inference with a skill template, budget-checked |
| `approval` | Human-in-the-loop: operator reviews via Slack before proceeding |

**AI never executes.** The LLM produces text. Deterministic code validates the output against a schema and decides what happens next.

Example pipeline:

```yaml
pipelines:
  - name: email-digest
    schedule: 30m
    steps:
      - name: fetch-unread
        type: deterministic
        action: email_unread

      - name: summarize
        type: ai
        skill: email-digest

      - name: report
        type: deterministic
        action: notify
```

## Guardrails

- **Token budgets**: per-step, per-pipeline, and per-day limits. Exceeding any budget halts the pipeline.
- **Rate limiting**: per-user, per-minute limits on operator interactions.
- **Input sanitization**: strips markdown and detects prompt injection patterns before forwarding to the LLM.
- **Output validation**: AI output is validated against the skill's output schema. Invalid output is rejected.
- **Human-in-the-loop**: approval steps present AI output to the operator via Slack with approve/edit/reject buttons. Nothing leaves the system without explicit approval.

## Configuration

### config.yaml

Defines your LLM providers, models, token budgets, and pipelines.

```yaml
provider:
  type: openrouter
  api_key_env: OPENROUTER_API_KEY
  base_url: https://openrouter.ai/api/v1

models:
  haiku:
    model: anthropic/claude-haiku-4-5
    max_tokens: 1024
  gpt-4o-mini:
    model: openai/gpt-4o-mini
    max_tokens: 1024

budgets:
  per_step_tokens: 2048
  per_pipeline_tokens: 10000
  per_day_tokens: 100000
```

### secrets.yaml

Private values that stay out of version control. Copy `secrets.yaml.example` to get started.

```yaml
# secrets.yaml.example
slack:
  channel_id: C0123456789
  allowed_users: [U0123456789]
```

### Skills

YAML prompt templates in `skills/`. Each skill defines the system prompt, input variables, and optional output schema for validation.

```yaml
# skills/classify-job.yaml
name: classify-job
system: |
  You are a job classifier. Given a job posting, determine if it matches
  the freelancer's profile. Return a JSON object with:
  - match: boolean
  - reason: string (one sentence)
  - score: number (0-100)
input_vars:
  - posting
  - profile
output_schema:
  type: object
  required: [match, reason, score]
```

## Project structure

```
fixclaw/
  main.go          # Engine: pipeline runner, Slack bot, scheduler, guardrails
  gmail.go         # Microsoft 365 / Gmail integration (OAuth 2.0, read + send with HITL approval)
  config.yaml      # Pipelines, models, budgets, timeouts
  secrets.yaml     # Private config (operator IDs) — gitignored
  skills/
    classify-job.yaml    # Job classification prompt template
    draft-followup.yaml  # Follow-up email drafter
    email-digest.yaml    # Unread email summarizer
```

## License

MIT. See [LICENSE](LICENSE).
