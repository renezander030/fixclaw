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

// --- Operator Channel Interface ---
// Each channel (TG, Slack, etc.) implements this interface.
// The engine doesn't know which channel it's talking to.

type OperatorChannel interface {
	// Send a plain notification (no approval needed)
	Send(text string) error
	// Send a draft for approval with action buttons. Returns the operator's decision.
	SendForApproval(ctx context.Context, draft string) (OperatorDecision, error)
}

type OperatorDecision struct {
	Action string // "approve", "skip", "adjust"
	Text   string // adjustment text (only if Action == "adjust")
}

// --- Telegram Channel ---

type TGBot struct {
	token       string
	chatID      int64
	offset      int
	security    ChannelSecurity
	rateLimiter *RateLimiter
}

func (t *TGBot) apiCall(method string, payload map[string]interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(payload)
	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", t.token, method),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	json.Unmarshal(respBody, &result)
	if !result.OK {
		return nil, fmt.Errorf("TG %s failed: %s", method, result.Description)
	}
	return result.Result, nil
}

func (t *TGBot) Send(text string) error {
	_, err := t.apiCall("sendMessage", map[string]interface{}{
		"chat_id": t.chatID,
		"text":    text,
	})
	return err
}

// sendWithButtons sends a message with inline keyboard buttons.
// Returns the message ID.
func (t *TGBot) sendWithButtons(text string, buttons [][]map[string]string) (int, error) {
	raw, err := t.apiCall("sendMessage", map[string]interface{}{
		"chat_id":      t.chatID,
		"text":         text,
		"reply_markup": map[string]interface{}{"inline_keyboard": buttons},
	})
	if err != nil {
		return 0, err
	}
	var msg struct {
		MessageID int `json:"message_id"`
	}
	json.Unmarshal(raw, &msg)
	return msg.MessageID, nil
}

// editButtons replaces the inline keyboard on an existing message.
func (t *TGBot) editButtons(msgID int, text string, buttons [][]map[string]string) {
	payload := map[string]interface{}{
		"chat_id":    t.chatID,
		"message_id": msgID,
		"text":       text,
	}
	if buttons != nil {
		payload["reply_markup"] = map[string]interface{}{"inline_keyboard": buttons}
	} else {
		// Remove keyboard
		payload["reply_markup"] = map[string]interface{}{"inline_keyboard": []interface{}{}}
	}
	t.apiCall("editMessageText", payload)
}

func (t *TGBot) answerCallback(callbackID string, text string) {
	t.apiCall("answerCallbackQuery", map[string]interface{}{
		"callback_query_id": callbackID,
		"text":              text,
	})
}

// SendForApproval posts a draft with Approve/Skip/Adjust buttons.
// Waits for the operator to click a button or send a text reply for adjustment.
func (t *TGBot) SendForApproval(ctx context.Context, draft string) (OperatorDecision, error) {
	buttons := [][]map[string]string{
		{
			{"text": "Approve", "callback_data": "approve"},
			{"text": "Skip", "callback_data": "skip"},
			{"text": "Adjust...", "callback_data": "adjust"},
		},
	}

	msgID, err := t.sendWithButtons(draft, buttons)
	if err != nil {
		return OperatorDecision{}, fmt.Errorf("failed to send draft: %w", err)
	}
	log.Printf("[telegram] draft posted (msg_id=%d), waiting for operator action...", msgID)

	// Poll for callback queries (button clicks) or text replies
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	waitingForText := false

	for {
		select {
		case <-ctx.Done():
			t.editButtons(msgID, draft+"\n\n[Timed out]", nil)
			return OperatorDecision{}, fmt.Errorf("approval timeout")
		case <-ticker.C:
			updates, err := t.getUpdates()
			if err != nil {
				continue
			}
			for _, u := range updates {
				// Handle callback query (button click)
				if u.CallbackQuery != nil {
					cb := u.CallbackQuery
					// Security: verify user
					if !t.isAllowedUser(cb.From.ID) {
						t.answerCallback(cb.ID, "Access denied.")
						log.Printf("[security] REJECTED callback from user %d", cb.From.ID)
						continue
					}
					if !t.rateLimiter.allow(cb.From.ID) {
						t.answerCallback(cb.ID, "Rate limited.")
						continue
					}
					// Must be for our message
					if cb.Message.MessageID != msgID {
						continue
					}

					switch cb.Data {
					case "approve":
						t.answerCallback(cb.ID, "Approved")
						t.editButtons(msgID, draft+"\n\n[Approved]", nil)
						return OperatorDecision{Action: "approve"}, nil
					case "skip":
						t.answerCallback(cb.ID, "Skipped")
						t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
						return OperatorDecision{Action: "skip"}, nil
					case "adjust":
						t.answerCallback(cb.ID, "Send your adjustment as a text message")
						waitingForText = true
						t.editButtons(msgID, draft+"\n\n[Waiting for adjustment text...]", nil)
					}
				}

				// Handle text message (adjustment)
				if waitingForText && u.Message.Text != "" {
					// Security checks
					if u.Message.Chat.ID != t.chatID {
						continue
					}
					if !t.isAllowedUser(u.Message.From.ID) {
						log.Printf("[security] REJECTED text from user %d", u.Message.From.ID)
						continue
					}
					if !t.rateLimiter.allow(u.Message.From.ID) {
						continue
					}

					// Input validation
					result := validateOperatorInput(u.Message.Text, t.security)
					if !result.Clean {
						log.Printf("[security] %s (user: %d)", result.Reason, u.Message.From.ID)
						t.Send("Input rejected: " + result.Reason)
						continue
					}

					return OperatorDecision{Action: "adjust", Text: result.Text}, nil
				}
			}
		}
	}
}

func (t *TGBot) isAllowedUser(userID int64) bool {
	for _, id := range t.security.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

type TGUpdate struct {
	UpdateID      int            `json:"update_id"`
	Message       *TGMessage     `json:"message"`
	CallbackQuery *TGCallback    `json:"callback_query"`
}

type TGMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	From      struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type TGCallback struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		MessageID int `json:"message_id"`
	} `json:"message"`
	Data string `json:"data"`
}

func (t *TGBot) getUpdates() ([]TGUpdate, error) {
	resp, err := http.Get(fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=1&allowed_updates=%s",
		t.token, t.offset, `["message","callback_query"]`,
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

func runPipeline(cfg *Config, pipeline PipelineConfig, budget *BudgetTracker, ch OperatorChannel) error {
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
			// Build draft message from AI output
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
				draftMsg = fmt.Sprintf("[aiops] %s - Score: %d/5\n\n%s\n\n%v",
					status, int(score), reason, data["input"])
			default:
				draftMsg = fmt.Sprintf("[aiops] Draft for review:\n\n%v", v)
			}

			// Send for approval via operator channel (TG buttons, Slack reactions, etc.)
			approvalTimeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
			if approvalTimeout == 0 {
				approvalTimeout = 4 * time.Hour
			}
			approvalCtx, approvalCancel := context.WithTimeout(ctx, approvalTimeout)
			decision, err := ch.SendForApproval(approvalCtx, draftMsg)
			approvalCancel()
			if err != nil {
				return fmt.Errorf("[step:%s] %w", step.Name, err)
			}

			log.Printf("[pipeline:%s][step:%s] operator decision: %s", pipeline.Name, step.Name, decision.Action)

			switch decision.Action {
			case "approve":
				data["approved"] = true
			case "skip":
				data["approved"] = false
				return nil
			case "adjust":
				log.Printf("[pipeline:%s][step:%s] adjustment: %q", pipeline.Name, step.Name, decision.Text)

				if err := budget.check(cfg.Budgets.PerDayTokens, cfg.Budgets.PerStepTokens); err != nil {
					ch.Send(fmt.Sprintf("Budget exceeded, cannot rewrite: %s", err))
					return err
				}

				adjustPrompt := fmt.Sprintf("Original output:\n%s\n\nOperator feedback:\n%s\n\nRewrite incorporating the feedback. Respond with ONLY valid JSON in the same format.", data["ai_raw"], decision.Text)

				aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
				if aiTimeout == 0 {
					aiTimeout = 30 * time.Second
				}
				aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)
				resp, err := callLLM(aiCtx, cfg, "drafter", adjustPrompt)
				aiCancel()
				if err != nil {
					ch.Send(fmt.Sprintf("Rewrite failed: %s", err))
					return err
				}
				budget.record(resp.InputTokens + resp.OutputTokens)

				// Show revised output and ask for final approval
				revisedDraft := fmt.Sprintf("[aiops] Revised:\n\n%s", resp.Text)
				approvalCtx2, approvalCancel2 := context.WithTimeout(ctx, approvalTimeout)
				decision2, err := ch.SendForApproval(approvalCtx2, revisedDraft)
				approvalCancel2()
				if err != nil {
					return err
				}
				if decision2.Action == "approve" {
					data["approved"] = true
				} else {
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
	bot.Send("[aiops] Engine started. Running pipeline: " + cfg.Pipelines[0].Name)

	if err := runPipeline(&cfg, cfg.Pipelines[0], budget, bot); err != nil {
		log.Printf("pipeline error: %v", err)
		bot.Send(fmt.Sprintf("[aiops] ERROR: %s", err))
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

