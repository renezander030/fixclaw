//go:build voice

package voice

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv("TEST_VOICE_TOKEN", "secret")

	cfg := Config{
		Enabled:    true,
		ListenAddr: "127.0.0.1:0",
		Auth:       AuthConfig{Method: "bearer", TokenEnv: "TEST_VOICE_TOKEN"},
		PreCall:    PreCallConfig{TimeoutMS: 300},
	}
	s, err := NewServer(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, s *Server, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, req)
	return w
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{"id": "s1"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestSessionStartHappy(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{
		"id":           "sess_1",
		"caller_phone": "+491234567890",
		"workflow":     "dach-standard",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSessionStartRequiresID(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestEventRequiresKind(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/event", map[string]any{"session_id": "sess_1"}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestOutcomePersistsAndCompletes(t *testing.T) {
	s := newTestServer(t)
	post(t, s, "/voice/session_start", map[string]any{"id": "sess_2", "workflow": "x"}, "secret")
	w := post(t, s, "/voice/session_end", map[string]any{
		"session_id":    "sess_2",
		"outcome":       map[string]string{"intent": "qualified"},
		"recording_url": "https://example/r.wav",
		"cost_cents":    1234,
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}

	row := s.store.db.QueryRow(`SELECT status, ended_at FROM voice_sessions WHERE id = 'sess_2'`)
	var status string
	var endedAt int64
	if err := row.Scan(&status, &endedAt); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("status want completed, got %s", status)
	}
	if endedAt == 0 {
		t.Fatal("ended_at should be set")
	}
}

func TestHandoffReturnsPendingTarget(t *testing.T) {
	s := newTestServer(t)
	post(t, s, "/voice/session_start", map[string]any{"id": "sess_h", "workflow": "x"}, "secret")
	w := post(t, s, "/voice/handoff", map[string]any{
		"session_id": "sess_h",
		"reason":     "complex VPC question",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["target"] != "pending_review" {
		t.Fatalf("want target=pending_review, got %v", got["target"])
	}
}

func TestLearningRequiresDescription(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/learning", map[string]any{"session_id": "sess_l"}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestPreCallReturnsDefaultWorkflow(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PreCall.RoutingRules = []RoutingRule{{Default: "standard-de"}}
	s.lookups = NewLookupRunner(s.cfg.PreCall)

	w := post(t, s, "/voice/pre_call_context", map[string]any{
		"caller_phone": "+491234567890",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var res PreCallResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Workflow != "standard-de" {
		t.Fatalf("want workflow=standard-de, got %q", res.Workflow)
	}
}

func TestBearerTokenValidation(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{"id": "s1"}, "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with wrong token, got %d", w.Code)
	}
}
