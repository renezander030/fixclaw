# Fixclaw

Your AI email assistant, production-ready in days, not months. Written in Go.

![Architecture](architecture.png)

## What it does

Fixclaw is a lightweight engine that runs AI-powered automation pipelines. Each pipeline is a sequence of steps: deterministic actions (fetch data, filter, route) and AI steps (classify, draft, summarize). AI never executes. It produces text. Deterministic code decides what happens next.

Every AI action goes through a human-in-the-loop approval flow before anything leaves the system. The operator reviews, edits, approves, or rejects via Slack.

## Design principles

- **Deterministic first.** Fetching, filtering, and routing are plain code. AI is only used for judgment calls: classification, drafting, summarization.
- **AI never executes.** The LLM produces text. Deterministic code validates the output against a schema and decides whether to act on it.
- **YAML-configured.** No code per automation. Define pipelines, steps, models, budgets, and skills in YAML.
- **Hardened by default.** Token budgets, rate limiting, input sanitization, output validation, and prompt injection defense are built in.

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

## Configuration

### config.yaml

The main config defines your LLM providers, models, token budgets, and pipelines.

```yaml
provider:
  type: openrouter
  api_key_env: OPENROUTER_API_KEY    # reads from environment variable
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

Skills are YAML prompt templates stored in `skills/`. Each skill defines the system prompt, input variables, and optional output schema for validation.

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

## Pipelines

A pipeline is a sequence of typed steps:

| Step type | What it does |
|---|---|
| `deterministic` | Plain code: fetch emails, filter data, send notifications |
| `ai` | LLM inference with a skill template, budget-checked |
| `approval` | Human-in-the-loop: operator reviews via Slack before proceeding |

Example pipeline from `config.yaml`:

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
- **Human-in-the-loop**: approval steps present the AI output to the operator via Slack with approve/edit/reject buttons. Nothing leaves the system without explicit approval.

## Setup

1. Clone the repo
2. Copy `secrets.yaml.example` to `secrets.yaml` and fill in your operator IDs
3. Set environment variables:
   ```
   export FIXCLAW_SLACK_TOKEN=your-slack-bot-token
   export OPENROUTER_API_KEY=your-openrouter-key
   ```
4. For email integration: configure your Microsoft 365 or Gmail OAuth token
5. Build and run:
   ```
   go build -o fixclaw .
   ./fixclaw
   ```

## License

MIT. See [LICENSE](LICENSE).
