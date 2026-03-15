package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// --- Config ---

type Config struct {
	Telegram TelegramConfig         `yaml:"telegram"`
	Provider ProviderConfig         `yaml:"provider"`
	Models   map[string]ModelConfig `yaml:"models"`
	Roles    map[string]string      `yaml:"roles"`
	Budgets  BudgetConfig           `yaml:"budgets"`
	Timeouts TimeoutConfig          `yaml:"timeouts"`
	Pipelines []PipelineConfig      `yaml:"pipelines"`
}

type TelegramConfig struct {
	TokenEnv string          `yaml:"token_env"`
	ChatID   int64           `yaml:"chat_id"`
	Security ChannelSecurity `yaml:"security"`
	token    string
}

// ChannelSecurity is REQUIRED per operator channel. Engine refuses to start without it.
type ChannelSecurity struct {
	AllowedUsers   []int64 `yaml:"allowed_users"`    // TG user IDs / Slack user IDs that may interact
	MaxInputLength int     `yaml:"max_input_length"`  // max chars per operator message (default 500)
	RateLimit      int     `yaml:"rate_limit"`         // max messages per minute per user (default 10)
	StripMarkdown  bool    `yaml:"strip_markdown"`     // strip formatting that could break prompt boundaries
}

type ProviderConfig struct {
	Type      string `yaml:"type"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
	apiKey    string
}

type ModelConfig struct {
	Model     string  `yaml:"model"`
	MaxTokens int     `yaml:"max_tokens"`
	CostIn    float64 `yaml:"cost_per_1k_input"`
	CostOut   float64 `yaml:"cost_per_1k_output"`
}

type BudgetConfig struct {
	PerStepTokens    int `yaml:"per_step_tokens"`
	PerPipelineTokens int `yaml:"per_pipeline_tokens"`
	PerDayTokens     int `yaml:"per_day_tokens"`
}

type TimeoutConfig struct {
	AICall           string `yaml:"ai_call"`
	OperatorApproval string `yaml:"operator_approval"`
	PipelineTotal    string `yaml:"pipeline_total"`
}

type PipelineConfig struct {
	Name     string       `yaml:"name"`
	Schedule string       `yaml:"schedule"`
	Steps    []StepConfig `yaml:"steps"`
}

type StepConfig struct {
	Name         string                 `yaml:"name"`
	Type         string                 `yaml:"type"` // deterministic, ai, approval
	Role         string                 `yaml:"role"`
	Prompt       string                 `yaml:"prompt"`
	Mode         string                 `yaml:"mode"`
	Channel      string                 `yaml:"channel"`
	OutputSchema map[string]interface{} `yaml:"output_schema"`
}

// --- Guardrails ---

type BudgetTracker struct {
	tokensUsedToday    int
	tokensUsedPipeline int
	dayStart           time.Time
}

func (b *BudgetTracker) check(limit int, requested int) error {
	if b.dayStart.Day() != time.Now().Day() {
		b.tokensUsedToday = 0
		b.dayStart = time.Now()
	}
	if b.tokensUsedToday+requested > limit {
		return fmt.Errorf("BUDGET_BLOCKED: daily token limit %d would be exceeded (used: %d, requested: %d)", limit, b.tokensUsedToday, requested)
	}
	return nil
}

func (b *BudgetTracker) record(tokens int) {
	b.tokensUsedToday += tokens
	b.tokensUsedPipeline += tokens
}

// --- Input Security ---
// Applied to ALL operator input before it reaches any AI step.
// This is not optional — the engine validates channel security config at startup.

// Prompt injection patterns — operator input is short commands/adjustments,
// never system prompts. Flag anything that looks like it's trying to rewrite
// the AI's instructions.
var injectionPatterns = []string{
	"ignore previous",
	"ignore above",
	"ignore all",
	"disregard",
	"forget your instructions",
	"you are now",
	"new instructions",
	"system prompt",
	"act as",
	"pretend to be",
	"jailbreak",
	"do anything now",
	"developer mode",
	"ignore safety",
	"bypass",
	"<|im_start|>",
	"<|im_end|>",
	"[INST]",
	"[/INST]",
	"<<SYS>>",
	"</s>",
	"\\n\\nHuman:",
	"\\n\\nAssistant:",
}

type InputValidationResult struct {
	Clean   bool
	Text    string
	Reason  string
}

func validateOperatorInput(text string, sec ChannelSecurity) InputValidationResult {
	// 1. Length check
	maxLen := sec.MaxInputLength
	if maxLen == 0 {
		maxLen = 500 // hard default
	}
	if len(text) > maxLen {
		return InputValidationResult{
			Clean:  false,
			Reason: fmt.Sprintf("INPUT_REJECTED: message too long (%d chars, max %d)", len(text), maxLen),
		}
	}

	// 2. Empty check
	text = strings.TrimSpace(text)
	if text == "" {
		return InputValidationResult{Clean: false, Reason: "INPUT_REJECTED: empty message"}
	}

	// 3. Prompt injection scan
	lower := strings.ToLower(text)
	for _, pattern := range injectionPatterns {
		if strings.Contains(lower, pattern) {
			return InputValidationResult{
				Clean:  false,
				Reason: fmt.Sprintf("INPUT_REJECTED: potential prompt injection detected (pattern: %q)", pattern),
			}
		}
	}

	// 4. Strip markdown/special chars that could break prompt boundaries
	if sec.StripMarkdown {
		text = strings.NewReplacer(
			"```", "",
			"~~~", "",
			"---", "",
			"===", "",
		).Replace(text)
	}

	// 5. Strip any attempt to inject role markers (XML-style or chat-style)
	text = stripRoleMarkers(text)

	return InputValidationResult{Clean: true, Text: text}
}

func stripRoleMarkers(text string) string {
	// Remove anything that looks like it's trying to inject system/assistant/user role boundaries
	replacer := strings.NewReplacer(
		"<system>", "",
		"</system>", "",
		"<assistant>", "",
		"</assistant>", "",
		"<user>", "",
		"</user>", "",
	)
	return replacer.Replace(text)
}

// validateChannelSecurity checks that security config is present and valid.
// Called at startup — engine refuses to start if this fails.
func validateChannelSecurity(cfg *Config) error {
	// Telegram channel must have security configured
	if cfg.Telegram.ChatID != 0 {
		sec := cfg.Telegram.Security
		if len(sec.AllowedUsers) == 0 {
			return fmt.Errorf("STARTUP_BLOCKED: telegram.security.allowed_users is required — specify which user IDs may interact with the bot")
		}
		if sec.MaxInputLength == 0 {
			cfg.Telegram.Security.MaxInputLength = 500 // enforce default
		}
		if sec.RateLimit == 0 {
			cfg.Telegram.Security.RateLimit = 10 // enforce default
		}
	}
	return nil
}

// RateLimiter tracks per-user message rates
type RateLimiter struct {
	windows map[int64][]time.Time // userID → timestamps of recent messages
	limit   int                   // max messages per minute
}

func newRateLimiter(limit int) *RateLimiter {
	if limit == 0 {
		limit = 10
	}
	return &RateLimiter{
		windows: make(map[int64][]time.Time),
		limit:   limit,
	}
}

func (r *RateLimiter) allow(userID int64) bool {
	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)

	// Prune old entries
	recent := []time.Time{}
	for _, t := range r.windows[userID] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= r.limit {
		return false
	}

	r.windows[userID] = append(recent, now)
	return true
}

// --- LLM Provider (OpenRouter) ---

type CompletionRequest struct {
	Model     string
	Prompt    string
	MaxTokens int
}

type CompletionResponse struct {
	Text         string
	InputTokens  int
	OutputTokens int
	LatencyMs    int64
	CostUSD      float64
	Model        string
}

func callLLM(ctx context.Context, cfg *Config, role string, prompt string) (*CompletionResponse, error) {
	modelName, ok := cfg.Roles[role]
	if !ok {
		return nil, fmt.Errorf("unknown role: %s", role)
	}
	modelCfg, ok := cfg.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("unknown model: %s", modelName)
	}

	reqBody := map[string]interface{}{
		"model": modelCfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": modelCfg.MaxTokens,
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.Provider.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Provider.apiKey)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()
	latency := time.Since(start).Milliseconds()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %w", err)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	cost := float64(result.Usage.PromptTokens)/1000*modelCfg.CostIn +
		float64(result.Usage.CompletionTokens)/1000*modelCfg.CostOut

	return &CompletionResponse{
		Text:         result.Choices[0].Message.Content,
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
		LatencyMs:    latency,
		CostUSD:      cost,
		Model:        result.Model,
	}, nil
}

// --- Output Validation ---

func toFloat64(v interface{}) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	default:
		return nil
	}
}

func validateOutput(text string, schema map[string]interface{}) (map[string]interface{}, error) {
	if len(schema) == 0 {
		return nil, nil
	}

	// Strip markdown code fences if present
	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		lines := strings.Split(cleaned, "\n")
		if len(lines) >= 3 {
			cleaned = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w\nRaw: %s", err, text)
	}

	for key, schemaDef := range schema {
		val, exists := parsed[key]
		if !exists {
			return nil, fmt.Errorf("missing required field: %s", key)
		}

		if defMap, ok := schemaDef.(map[string]interface{}); ok {
			if typeName, ok := defMap["type"].(string); ok {
				switch typeName {
				case "int":
					num := toFloat64(val)
					if num == nil {
						return nil, fmt.Errorf("field %s: expected number, got %T", key, val)
					}
					if minVal, ok := defMap["min"]; ok {
						if mv := toFloat64(minVal); mv != nil && *num < *mv {
							return nil, fmt.Errorf("field %s: value %v below min %v", key, *num, *mv)
						}
					}
					if maxVal, ok := defMap["max"]; ok {
						if mv := toFloat64(maxVal); mv != nil && *num > *mv {
							return nil, fmt.Errorf("field %s: value %v above max %v", key, *num, *mv)
						}
					}
				case "bool":
					if _, ok := val.(bool); !ok {
						return nil, fmt.Errorf("field %s: expected bool, got %T", key, val)
					}
				case "string":
					if _, ok := val.(string); !ok {
						return nil, fmt.Errorf("field %s: expected string, got %T", key, val)
					}
				}
			}
		}
	}

	return parsed, nil
}

// --- Telegram ---

type TGBot struct {
	token       string
	chatID      int64
	offset      int
	security    ChannelSecurity
	rateLimiter *RateLimiter
}

func (t *TGBot) send(text string) (int, error) {
	reqBody := map[string]interface{}{
		"chat_id":    t.chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.Unmarshal(respBody, &result)
	if !result.OK {
		return 0, fmt.Errorf("TG send failed: %s", string(respBody))
	}
	return result.Result.MessageID, nil
}

func (t *TGBot) sendReply(text string, replyTo int) (int, error) {
	reqBody := map[string]interface{}{
		"chat_id":              t.chatID,
		"text":                 text,
		"parse_mode":           "Markdown",
		"reply_to_message_id":  replyTo,
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.Unmarshal(respBody, &result)
	return result.Result.MessageID, nil
}

// waitForReply polls for a reply to a specific message. Returns the reply text.
// All operator input passes through security validation before being returned.
func (t *TGBot) waitForReply(ctx context.Context, msgID int) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("approval timeout")
		case <-ticker.C:
			updates, err := t.getUpdates()
			if err != nil {
				continue
			}
			for _, u := range updates {
				// 1. Chat ID check — wrong chat, silently ignore
				if u.Message.Chat.ID != t.chatID {
					continue
				}

				// 2. User ID check — must be in allowed_users list
				userID := u.Message.From.ID
				allowed := false
				for _, id := range t.security.AllowedUsers {
					if id == userID {
						allowed = true
						break
					}
				}
				if !allowed {
					log.Printf("[security] REJECTED: user %d not in allowed_users", userID)
					t.sendReply("Access denied. Your user ID is not authorized.", u.Message.MessageID)
					continue
				}

				// 3. Rate limit check
				if !t.rateLimiter.allow(userID) {
					log.Printf("[security] RATE_LIMITED: user %d exceeded %d msg/min", userID, t.rateLimiter.limit)
					t.sendReply("Rate limited. Please wait before sending another message.", u.Message.MessageID)
					continue
				}

				// Must be a reply to our message
				if u.Message.ReplyToMessage == nil || u.Message.ReplyToMessage.MessageID != msgID {
					continue
				}

				// 4. Input validation — length, injection patterns, sanitization
				result := validateOperatorInput(u.Message.Text, t.security)
				if !result.Clean {
					log.Printf("[security] %s (user: %d, text: %q)", result.Reason, userID, u.Message.Text)
					t.sendReply("Input rejected. "+result.Reason, u.Message.MessageID)
					continue
				}

				return result.Text, nil
			}
		}
	}
}

type TGUpdate struct {
	UpdateID int `json:"update_id"`
	Message  struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		ReplyToMessage *struct {
			MessageID int `json:"message_id"`
		} `json:"reply_to_message"`
	} `json:"message"`
}

func (t *TGBot) getUpdates() ([]TGUpdate, error) {
	resp, err := http.Get(fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=1",
		t.token, t.offset,
	))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		OK     bool       `json:"ok"`
		Result []TGUpdate `json:"result"`
	}
	json.Unmarshal(body, &result)

	if len(result.Result) > 0 {
		t.offset = result.Result[len(result.Result)-1].UpdateID + 1
	}

	return result.Result, nil
}

// --- Pipeline Engine ---

func runPipeline(cfg *Config, pipeline PipelineConfig, budget *BudgetTracker, bot *TGBot) error {
	log.Printf("[pipeline:%s] starting", pipeline.Name)
	budget.tokensUsedPipeline = 0

	// Parse pipeline timeout
	pipelineTimeout, _ := time.ParseDuration(cfg.Timeouts.PipelineTotal)
	if pipelineTimeout == 0 {
		pipelineTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), pipelineTimeout)
	defer cancel()

	// Pipeline data flows through this map
	data := map[string]interface{}{}

	// Mock test data
	data["input"] = `Title: Senior AI/LLM Engineer - Build RAG Pipeline for Legal Documents
Rate: $80-120/hr
Duration: 3-6 months, 20 hrs/week
Client: $45K spent, 4.92 stars, United States
Skills: RAG, Vector Databases, Claude API, TypeScript
Description: We need an experienced LLM engineer to build a retrieval-augmented generation pipeline for searching and summarizing legal documents. Must have production RAG experience.`

	for _, step := range pipeline.Steps {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pipeline timeout after %s", pipelineTimeout)
		default:
		}

		log.Printf("[pipeline:%s][step:%s] type=%s", pipeline.Name, step.Name, step.Type)

		switch step.Type {
		case "deterministic":
			// Pass-through for now. In production: fetch, filter, transform.
			log.Printf("[pipeline:%s][step:%s] deterministic pass-through", pipeline.Name, step.Name)

		case "ai":
			// Budget pre-flight
			if err := budget.check(cfg.Budgets.PerDayTokens, cfg.Budgets.PerStepTokens); err != nil {
				log.Printf("[pipeline:%s][step:%s] %s", pipeline.Name, step.Name, err)
				return err
			}

			// Build prompt with template substitution
			prompt := step.Prompt
			for k, v := range data {
				prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", fmt.Sprintf("%v", v))
			}

			// AI call with timeout
			aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
			if aiTimeout == 0 {
				aiTimeout = 30 * time.Second
			}
			aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)

			resp, err := callLLM(aiCtx, cfg, step.Role, prompt)
			aiCancel()
			if err != nil {
				return fmt.Errorf("[step:%s] LLM call failed: %w", step.Name, err)
			}

			// Record token usage
			budget.record(resp.InputTokens + resp.OutputTokens)
			log.Printf("[pipeline:%s][step:%s] model=%s tokens=%d+%d cost=$%.4f latency=%dms",
				pipeline.Name, step.Name, resp.Model,
				resp.InputTokens, resp.OutputTokens, resp.CostUSD, resp.LatencyMs)

			// Validate output
			if len(step.OutputSchema) > 0 {
				parsed, err := validateOutput(resp.Text, step.OutputSchema)
				if err != nil {
					log.Printf("[pipeline:%s][step:%s] OUTPUT_INVALID: %s", pipeline.Name, step.Name, err)
					return fmt.Errorf("[step:%s] output validation failed: %w", step.Name, err)
				}
				data["ai_output"] = parsed
				log.Printf("[pipeline:%s][step:%s] output validated: %v", pipeline.Name, step.Name, parsed)
			} else {
				data["ai_output"] = resp.Text
			}
			data["ai_raw"] = resp.Text

		case "approval":
			// Post draft to operator channel
			aiOutput := data["ai_output"]
			var draftMsg string

			switch v := aiOutput.(type) {
			case map[string]interface{}:
				score, _ := v["score"].(float64)
				reason, _ := v["reason"].(string)
				reject, _ := v["reject"].(bool)
				status := "MATCH"
				if reject {
					status = "REJECT"
				}
				draftMsg = fmt.Sprintf("*[aiops] %s — Score: %d/5*\n\n%s\n\n_%s_\n\nReply: `ok` to approve, `skip` to skip, or any text to adjust",
					status, int(score), reason, fmt.Sprintf("%v", data["input"]))
			default:
				draftMsg = fmt.Sprintf("*[aiops] Draft for review:*\n\n%v\n\nReply: `ok` to approve, `skip` to skip, or any text to adjust", v)
			}

			msgID, err := bot.send(draftMsg)
			if err != nil {
				return fmt.Errorf("[step:%s] failed to send to operator: %w", step.Name, err)
			}
			log.Printf("[pipeline:%s][step:%s] posted to TG (msg_id=%d), waiting for reply...", pipeline.Name, step.Name, msgID)

			// Wait for operator reply with timeout
			approvalTimeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
			if approvalTimeout == 0 {
				approvalTimeout = 4 * time.Hour
			}
			approvalCtx, approvalCancel := context.WithTimeout(ctx, approvalTimeout)

			reply, err := bot.waitForReply(approvalCtx, msgID)
			approvalCancel()
			if err != nil {
				bot.sendReply("Approval timed out. Skipping.", msgID)
				return fmt.Errorf("[step:%s] %w", step.Name, err)
			}

			reply = strings.TrimSpace(strings.ToLower(reply))
			log.Printf("[pipeline:%s][step:%s] operator replied: %q", pipeline.Name, step.Name, reply)

			switch reply {
			case "ok", "yes", "approve":
				bot.sendReply("Approved. Executing.", msgID)
				data["approved"] = true
			case "skip", "no", "reject":
				bot.sendReply("Skipped.", msgID)
				data["approved"] = false
				log.Printf("[pipeline:%s][step:%s] operator skipped", pipeline.Name, step.Name)
				return nil // End pipeline
			default:
				// Operator wants adjustment — rewrite with AI
				log.Printf("[pipeline:%s][step:%s] operator requested adjustment: %q", pipeline.Name, step.Name, reply)

				if err := budget.check(cfg.Budgets.PerDayTokens, cfg.Budgets.PerStepTokens); err != nil {
					bot.sendReply(fmt.Sprintf("Budget exceeded, cannot rewrite: %s", err), msgID)
					return err
				}

				adjustPrompt := fmt.Sprintf("Original output:\n%s\n\nOperator feedback:\n%s\n\nRewrite the output incorporating the feedback. Respond with ONLY valid JSON in the same format.", data["ai_raw"], reply)

				aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
				if aiTimeout == 0 {
					aiTimeout = 30 * time.Second
				}
				aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)
				resp, err := callLLM(aiCtx, cfg, "drafter", adjustPrompt)
				aiCancel()
				if err != nil {
					bot.sendReply(fmt.Sprintf("Rewrite failed: %s", err), msgID)
					return err
				}
				budget.record(resp.InputTokens + resp.OutputTokens)

				bot.sendReply(fmt.Sprintf("*Revised:*\n%s\n\nReply `ok` to approve or `skip`", resp.Text), msgID)

				// Wait for second approval
				approvalCtx2, approvalCancel2 := context.WithTimeout(ctx, approvalTimeout)
				reply2, err := bot.waitForReply(approvalCtx2, msgID)
				approvalCancel2()
				if err != nil {
					return err
				}
				reply2 = strings.TrimSpace(strings.ToLower(reply2))
				if reply2 == "ok" || reply2 == "yes" || reply2 == "approve" {
					bot.sendReply("Approved. Executing.", msgID)
					data["approved"] = true
				} else {
					bot.sendReply("Skipped.", msgID)
					return nil
				}
			}
		}
	}

	log.Printf("[pipeline:%s] completed. tokens_used=%d", pipeline.Name, budget.tokensUsedPipeline)
	return nil
}

// --- Main ---

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// Load config
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("failed to read config: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}

	// Resolve env vars
	cfg.Telegram.token = resolveEnv(cfg.Telegram.TokenEnv, "AIOPS_TG_TOKEN")
	cfg.Provider.apiKey = resolveEnv(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY")

	if cfg.Telegram.token == "" {
		log.Fatal("Telegram token not set. Set AIOPS_TG_TOKEN env var.")
	}
	if cfg.Provider.apiKey == "" {
		log.Fatal("OpenRouter API key not set. Set OPENROUTER_API_KEY env var.")
	}

	// Validate channel security — refuse to start without it
	if err := validateChannelSecurity(&cfg); err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("aiops starting — %d pipeline(s), provider=%s, operator=telegram:%d, allowed_users=%v",
		len(cfg.Pipelines), cfg.Provider.Type, cfg.Telegram.ChatID, cfg.Telegram.Security.AllowedUsers)

	bot := &TGBot{
		token:       cfg.Telegram.token,
		chatID:      cfg.Telegram.ChatID,
		security:    cfg.Telegram.Security,
		rateLimiter: newRateLimiter(cfg.Telegram.Security.RateLimit),
	}
	budget := &BudgetTracker{dayStart: time.Now()}

	// Drain pending updates so we don't process old messages
	bot.getUpdates()

	// For now: run first pipeline once (manual trigger)
	if len(cfg.Pipelines) == 0 {
		log.Fatal("no pipelines configured")
	}

	// Send startup notification
	bot.send("*[aiops]* Engine started. Running pipeline: `" + cfg.Pipelines[0].Name + "`")

	if err := runPipeline(&cfg, cfg.Pipelines[0], budget, bot); err != nil {
		log.Printf("pipeline error: %v", err)
		bot.send(fmt.Sprintf("*[aiops] ERROR:* `%s`", err))
	}

	log.Printf("aiops done. daily tokens used: %d", budget.tokensUsedToday)
}

func resolveEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

