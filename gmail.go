package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// --- Gmail Connector ---
// Outbound only. Polls Gmail API via OAuth. Read-only by default.

type GmailConfig struct {
	TokenPath  string `yaml:"token_path"`
	Scopes     string `yaml:"scopes"` // enforced: gmail.readonly unless explicitly changed
	Permission string `yaml:"permission"` // read | draft | send
}

type GmailToken struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	TokenURI     string `json:"token_uri"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Expiry       string `json:"expiry"`
}

type GmailConnector struct {
	token     GmailToken
	tokenPath string
	accessToken string
	expiresAt   time.Time
}

type Email struct {
	ID      string
	From    string
	To      string
	Subject string
	Date    string
	Snippet string
	Body    string
	Labels  []string
}

func NewGmailConnector(tokenPath string) (*GmailConnector, error) {
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read gmail token: %w", err)
	}
	var token GmailToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("failed to parse gmail token: %w", err)
	}
	g := &GmailConnector{
		token:     token,
		tokenPath: tokenPath,
	}
	// Get initial access token
	if err := g.refreshAccessToken(); err != nil {
		return nil, fmt.Errorf("failed to get access token: %w", err)
	}
	log.Printf("[gmail] connector initialized")
	return g, nil
}

func (g *GmailConnector) refreshAccessToken() error {
	data := url.Values{
		"client_id":     {g.token.ClientID},
		"client_secret": {g.token.ClientSecret},
		"refresh_token": {g.token.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := http.PostForm(g.token.TokenURI, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	json.Unmarshal(body, &result)
	if result.Error != "" {
		return fmt.Errorf("token refresh failed: %s", result.Error)
	}
	g.accessToken = result.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)

	// Save refreshed token back
	g.token.Token = result.AccessToken
	tokenData, _ := json.MarshalIndent(g.token, "", "  ")
	os.WriteFile(g.tokenPath, tokenData, 0600)

	log.Printf("[gmail] access token refreshed, expires in %ds", result.ExpiresIn)
	return nil
}

func (g *GmailConnector) ensureToken() error {
	if time.Now().After(g.expiresAt) {
		return g.refreshAccessToken()
	}
	return nil
}

func (g *GmailConnector) apiGet(endpoint string) (json.RawMessage, error) {
	if err := g.ensureToken(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "https://gmail.googleapis.com/gmail/v1/users/me/"+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+g.accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gmail API %d: %s", resp.StatusCode, string(body)[:min(200, len(body))])
	}
	return body, nil
}

// FetchRecent fetches the N most recent emails matching a query
func (g *GmailConnector) FetchRecent(query string, maxResults int) ([]Email, error) {
	if maxResults == 0 {
		maxResults = 10
	}
	q := url.QueryEscape(query)
	endpoint := fmt.Sprintf("messages?q=%s&maxResults=%d", q, maxResults)
	raw, err := g.apiGet(endpoint)
	if err != nil {
		return nil, err
	}

	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	json.Unmarshal(raw, &list)

	var emails []Email
	for _, m := range list.Messages {
		email, err := g.GetMessage(m.ID)
		if err != nil {
			log.Printf("[gmail] failed to get message %s: %v", m.ID, err)
			continue
		}
		emails = append(emails, *email)
	}
	return emails, nil
}

// GetMessage fetches a single email by ID
func (g *GmailConnector) GetMessage(id string) (*Email, error) {
	raw, err := g.apiGet("messages/" + id + "?format=full")
	if err != nil {
		return nil, err
	}

	var msg struct {
		ID      string `json:"id"`
		Snippet string `json:"snippet"`
		Payload struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
			Parts []struct {
				MimeType string `json:"mimeType"`
				Body     struct {
					Data string `json:"data"`
				} `json:"body"`
			} `json:"parts"`
			Body struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"payload"`
		LabelIds []string `json:"labelIds"`
	}
	json.Unmarshal(raw, &msg)

	email := Email{
		ID:      msg.ID,
		Snippet: msg.Snippet,
		Labels:  msg.LabelIds,
	}

	// Extract headers
	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "From":
			email.From = h.Value
		case "To":
			email.To = h.Value
		case "Subject":
			email.Subject = h.Value
		case "Date":
			email.Date = h.Value
		}
	}

	// Extract body
	if len(msg.Payload.Parts) > 0 {
		for _, part := range msg.Payload.Parts {
			if part.MimeType == "text/plain" && part.Body.Data != "" {
				decoded, _ := base64.URLEncoding.DecodeString(part.Body.Data)
				email.Body = string(decoded)
				break
			}
		}
	} else if msg.Payload.Body.Data != "" {
		decoded, _ := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		email.Body = string(decoded)
	}

	// Truncate body for token efficiency
	if len(email.Body) > 500 {
		email.Body = email.Body[:500] + "..."
	}

	return &email, nil
}

// FetchUnread fetches unread emails
func (g *GmailConnector) FetchUnread(maxResults int) ([]Email, error) {
	return g.FetchRecent("is:unread", maxResults)
}

// FormatForPrompt turns emails into a compact string for LLM consumption
func FormatEmailsForPrompt(emails []Email) string {
	if len(emails) == 0 {
		return "No emails found."
	}
	var sb strings.Builder
	for i, e := range emails {
		// Clean up from field
		from := e.From
		if idx := strings.Index(from, "<"); idx > 0 {
			from = strings.TrimSpace(from[:idx])
		}
		from = strings.Trim(from, "\"")

		subject := e.Subject
		if len(subject) > 80 {
			subject = subject[:80] + "..."
		}

		sb.WriteString(fmt.Sprintf("%d. %s | %s\n   %s\n", i+1, from, subject, e.Snippet))
		if i < len(emails)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// ExtractEmailAddress pulls email from "Name <email>" format
func ExtractEmailAddress(from string) string {
	re := regexp.MustCompile(`<([^>]+)>`)
	matches := re.FindStringSubmatch(from)
	if len(matches) > 1 {
		return matches[1]
	}
	return from
}
