//go:build voice

package voice

type CompletedSession struct {
	ID             string
	CallerPhone    string
	Workflow       string
	StartedAt      int64
	EndedAt        int64
	OutcomeJSON    string
	TranscriptJSON string
	RecordingURL   string
	CostCents      int64
}

func (s *Store) FetchCompletedSessions(limit int) ([]CompletedSession, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT s.id, COALESCE(s.caller_phone, ''), COALESCE(s.workflow, ''),
		       s.started_at, COALESCE(s.ended_at, 0),
		       o.outcome_json, COALESCE(o.transcript_json, ''),
		       COALESCE(o.recording_url, ''), COALESCE(o.cost_cents, 0)
		  FROM voice_sessions s
		  JOIN voice_outcomes o ON o.session_id = s.id
		 WHERE s.status = 'completed'
		 ORDER BY s.ended_at DESC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletedSession
	for rows.Next() {
		var c CompletedSession
		if err := rows.Scan(&c.ID, &c.CallerPhone, &c.Workflow,
			&c.StartedAt, &c.EndedAt,
			&c.OutcomeJSON, &c.TranscriptJSON, &c.RecordingURL, &c.CostCents); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type PendingHandoff struct {
	ID          int64
	SessionID   string
	RequestedAt int64
	Reason      string
}

func (s *Store) FetchPendingHandoffs(limit int) ([]PendingHandoff, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, requested_at, COALESCE(reason, '')
		  FROM voice_handoffs
		 WHERE resolved_at IS NULL
		 ORDER BY requested_at ASC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingHandoff
	for rows.Next() {
		var h PendingHandoff
		if err := rows.Scan(&h.ID, &h.SessionID, &h.RequestedAt, &h.Reason); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) ResolveHandoff(id int64, target string, resolvedAt int64) error {
	_, err := s.db.Exec(`
		UPDATE voice_handoffs
		   SET target_resolved = ?, resolved_at = ?
		 WHERE id = ?
	`, target, resolvedAt, id)
	return err
}

type NewLearning struct {
	ID          int64
	SessionID   string
	TS          int64
	Category    string
	Description string
	Severity    string
}

func (s *Store) FetchNewLearnings(limit int) ([]NewLearning, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, ts, COALESCE(category, ''), description, COALESCE(severity, '')
		  FROM voice_learnings
		 WHERE review_status = 'new'
		 ORDER BY ts ASC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NewLearning
	for rows.Next() {
		var l NewLearning
		if err := rows.Scan(&l.ID, &l.SessionID, &l.TS, &l.Category, &l.Description, &l.Severity); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) MarkLearningStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE voice_learnings SET review_status = ? WHERE id = ?`, status, id)
	return err
}
