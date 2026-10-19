package main

// Opslag van gesprekken in SQLite (modernc.org/sqlite: pure Go, geen cgo), in data/conversations.db.
//
// Per gesprek één rij met:
//   - agent:      wat het model nodig heeft om verder te gaan (geschiedenis, samenvatting, actielijst)
//   - transcript: wat de pagina toont (de events), ingekort door opeenvolgende tokens samen te voegen
//
// Opslaan gebeurt aan het eind van elk antwoord. Valt de server midden in een antwoord weg,
// dan mis je alleen dat ene antwoord.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type convStore struct{ db *sql.DB }

type convSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated int64  `json:"updated"` // unix ms
	Model   string `json:"model"`
}

// agentState is wat er van een Agent bewaard wordt.
type agentState struct {
	History []Message `json:"history"`
	Summary string    `json:"summary,omitempty"`
	Ledger  []string  `json:"ledger,omitempty"`
}

func (a *Agent) snapshot() agentState {
	return agentState{History: a.History, Summary: a.summary, Ledger: a.ledger}
}

func (a *Agent) restore(st agentState) {
	a.summary, a.ledger = st.Summary, st.Ledger
	a.History = st.History
	if len(a.History) == 0 || a.History[0].Role != "system" {
		a.History = append([]Message{{Role: "system"}}, a.History...)
	}
	a.History[0] = a.systemMessage() // altijd de actuele systeemprompt
	a.measured = 0
}

// migrateDB hernoemt een database van vóór de naamswijziging (llamatest → go-local-agent),
// zodat bestaande gesprekken bewaard blijven. Doet niets als de nieuwe al bestaat.
func migrateDB(oldPath, newPath string) {
	if _, err := os.Stat(newPath); err == nil {
		return
	}
	if _, err := os.Stat(oldPath); err != nil {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Rename(oldPath+suffix, newPath+suffix)
	}
}

func openStore(path string) (*convStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: één schrijver tegelijk; zo hoeft niemand te wachten op locks
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS conversations (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		created    INTEGER NOT NULL,
		updated    INTEGER NOT NULL,
		model      TEXT NOT NULL DEFAULT '',
		agent      TEXT NOT NULL,
		transcript TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS conversations_updated ON conversations(updated DESC);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &convStore{db: db}, nil
}

// save schrijft een gesprek weg. Een titel die je zelf gegeven hebt, blijft staan.
func (cs *convStore) save(id, title, model string, st agentState, events []Event) error {
	agent, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tr, err := json.Marshal(events)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = cs.db.Exec(`INSERT INTO conversations (id, title, created, updated, model, agent, transcript)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET updated = excluded.updated, model = excluded.model,
			agent = excluded.agent, transcript = excluded.transcript,
			title = CASE WHEN conversations.title = '' THEN excluded.title ELSE conversations.title END`,
		id, title, now, now, model, string(agent), string(tr))
	return err
}

func (cs *convStore) load(id string) (agentState, []Event, bool) {
	var agent, tr string
	err := cs.db.QueryRow(`SELECT agent, transcript FROM conversations WHERE id = ?`, id).Scan(&agent, &tr)
	if err != nil {
		return agentState{}, nil, false
	}
	var st agentState
	var events []Event
	if json.Unmarshal([]byte(agent), &st) != nil || json.Unmarshal([]byte(tr), &events) != nil {
		return agentState{}, nil, false
	}
	return st, events, true
}

func (cs *convStore) list(limit int) ([]convSummary, error) {
	rows, err := cs.db.Query(`SELECT id, title, updated, model FROM conversations ORDER BY updated DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []convSummary{}
	for rows.Next() {
		var c convSummary
		if err := rows.Scan(&c.ID, &c.Title, &c.Updated, &c.Model); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (cs *convStore) rename(id, title string) error {
	_, err := cs.db.Exec(`UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

func (cs *convStore) delete(id string) error {
	_, err := cs.db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	return err
}

// compactEvents voegt opeenvolgende token- en denk-events samen: een antwoord van 800 tokens
// wordt zo één event in plaats van 800. Status- en hartslag-events zijn niet nodig om terug te kijken.
func compactEvents(in []Event) []Event {
	out := make([]Event, 0, len(in)/4)
	for _, e := range in {
		switch e.Type {
		case "status", "hb", "info", "taken":
			continue
		}
		e.Seq = 0
		if n := len(out); n > 0 && (e.Type == "token" || e.Type == "thinking") && out[n-1].Type == e.Type {
			out[n-1].Text += e.Text
			continue
		}
		out = append(out, e)
	}
	return out
}

// titleFrom maakt een titel van de eerste vraag (zoals Claude: kort en herkenbaar).
func titleFrom(events []Event) string {
	for _, e := range events {
		if e.Type != "user" {
			continue
		}
		t := strings.Join(strings.Fields(e.Text), " ")
		if t == "" && len(e.Files) > 0 {
			t = e.Files[0]
		}
		if r := []rune(t); len(r) > 60 {
			t = strings.TrimSpace(string(r[:57])) + "…"
		}
		return t
	}
	return ""
}
