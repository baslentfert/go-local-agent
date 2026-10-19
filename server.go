package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
)

//go:embed web/index.html
var indexHTML []byte

const (
	heartbeatEvery = 20 * time.Second // beide kanten sturen een "hb" zodat dode verbindingen opvallen
	readTimeout    = 50 * time.Second
	writeTimeout   = 10 * time.Second
	sessionIdle    = 30 * time.Minute // zo lang blijft een losgekoppelde sessie bewaard
	maxTranscript  = 50000
)

// clientMsg is wat de browser over de websocket stuurt.
type clientMsg struct {
	Type     string         `json:"type"` // prompt, confirm, stop, reset, compact, hb
	Text     string         `json:"text"`
	OK       bool           `json:"ok"`
	Files    []upload       `json:"files"`    // bijlagen bij een prompt
	Rejected []rejectedFile `json:"rejected"` // bijlagen die de pagina al weigerde (ook die moet het model kennen)
}

// session is één gesprek. Het overleeft het wegvallen van de websocket: de agent loopt door,
// alle events krijgen een volgnummer en worden bewaard, zodat een (her)verbonden pagina
// precies kan ophalen wat ze gemist heeft.
type session struct {
	id      string
	ag      *Agent
	ctx     context.Context
	end     context.CancelFunc
	running atomic.Bool
	deleted atomic.Bool // verwijderd uit de lijst: niet meer opslaan, ook niet bij een laatste "done"
	confirm chan bool

	mu         sync.Mutex // beschermt alles hieronder en serialiseert writes naar conn
	conn       *websocket.Conn
	seq        int
	transcript []Event
	cancelAsk  context.CancelFunc
	detachedAt time.Time
}

var sessions = struct {
	sync.Mutex
	m map[string]*session
}{m: map[string]*session{}}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var store *convStore // gesprekken op schijf; nil = niet opslaan

// getSession zoekt een gesprek: eerst in het geheugen (resumed=true: de pagina krijgt alleen
// wat ze mist), dan in de database (de pagina krijgt alles opnieuw), anders een nieuw gesprek.
func getSession(id string, newAgent func() *Agent) (*session, bool) {
	sessions.Lock()
	defer sessions.Unlock()
	if s, ok := sessions.m[id]; ok {
		return s, true
	}
	if id != "" && store != nil {
		if st, events, ok := store.load(id); ok {
			s := newSession(id, newAgent())
			s.ag.restore(st)
			for _, e := range events {
				s.seq++
				e.Seq = s.seq
				s.transcript = append(s.transcript, e)
			}
			sessions.m[id] = s
			s.ag.emitContext()
			return s, false
		}
	}
	s := newSession(newID(), newAgent())
	sessions.m[s.id] = s
	s.ag.emitContext() // meter meteen tonen (systeemprompt + tools tellen al mee)
	return s, false
}

func newSession(id string, ag *Agent) *session {
	s := &session{id: id, ag: ag, confirm: make(chan bool), cancelAsk: func() {}}
	s.ctx, s.end = context.WithCancel(context.Background())
	s.ag.Emit = s.emit
	s.ag.Confirm = func(q string) bool {
		s.emit(Event{Type: "confirm", Text: q})
		ok := false
		select {
		case ok = <-s.confirm:
		case <-s.ctx.Done():
		}
		s.emit(Event{Type: "confirmed", Text: q, OK: ok})
		return ok
	}
	return s
}

// saveLocked bewaart het gesprek (aanroepen met s.mu vast). Gesprekken zonder vraag slaan we
// niet op, anders staat de lijst vol lege "nieuwe gesprekken".
func (s *session) saveLocked() {
	if store == nil || s.deleted.Load() {
		return
	}
	title := titleFrom(s.transcript)
	if title == "" {
		return
	}
	if err := store.save(s.id, title, s.ag.Model, s.ag.snapshot(), compactEvents(s.transcript)); err != nil {
		log.Printf("gesprek %s opslaan: %v", s.id, err)
	}
}

// cleanupSessions ruimt sessies op die lang los hangen en niets meer doen.
func cleanupSessions() {
	for range time.Tick(time.Minute) {
		sessions.Lock()
		for id, s := range sessions.m {
			s.mu.Lock()
			idle := s.conn == nil && time.Since(s.detachedAt) > sessionIdle
			s.mu.Unlock()
			if idle && !s.running.Load() {
				s.end()
				delete(sessions.m, id)
			}
		}
		sessions.Unlock()
	}
}

func (s *session) write(e Event) error {
	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return s.conn.WriteJSON(e)
}

// emit nummert een event, bewaart het en stuurt het door als er een verbinding is.
func (s *session) emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq = s.seq
	s.transcript = append(s.transcript, e)
	if len(s.transcript) > maxTranscript {
		s.transcript = s.transcript[len(s.transcript)-maxTranscript*4/5:]
	}
	// Eerst opslaan, dan pas "done" naar de pagina: die haalt bij "done" de lijst op, en dan
	// moet het gesprek er al in staan. ("done" komt van de goroutine van de agent zelf, dus de
	// geschiedenis staat op dat moment stil.)
	if e.Type == "done" {
		s.saveLocked()
	}
	if s.conn != nil {
		s.write(e) // bij een fout merkt de leeslus het en koppelen we los
	}
}

// attach koppelt een nieuwe verbinding en stuurt alles na wat na `since` gebeurd is.
func (s *session) attach(c *websocket.Conn, since int, resumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.conn; old != nil && old != c {
		s.write(Event{Type: "taken"}) // ander tabblad neemt het gesprek over
		old.Close()
	}
	s.conn = c
	if !resumed {
		since = 0
	}
	s.write(Event{Type: "info", Text: s.ag.Model, SID: s.id, Resume: resumed, Busy: s.running.Load()})
	for _, e := range s.transcript {
		if e.Seq > since {
			s.write(e)
		}
	}
}

func (s *session) detach(c *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == c {
		s.conn = nil
		s.detachedAt = time.Now()
	}
}

func (s *session) handle(m clientMsg) {
	switch m.Type {
	case "prompt":
		if !s.running.CompareAndSwap(false, true) {
			s.emit(Event{Type: "error", Text: "nog bezig met de vorige vraag"})
			return
		}
		var names []string
		for _, f := range m.Files {
			if f.Data != "" {
				size := len(f.Data) * 3 / 4 // base64 → bytes
				if size >= 1024*1024 {
					names = append(names, fmt.Sprintf("%s (%.1f MB)", f.Name, float64(size)/1024/1024))
				} else {
					names = append(names, fmt.Sprintf("%s (%d KB)", f.Name, (size+1023)/1024))
				}
			} else {
				names = append(names, fmt.Sprintf("%s (%d tekens)", f.Name, len(f.Content)))
			}
		}
		for _, r := range m.Rejected {
			names = append(names, fmt.Sprintf("⚠ %s (geweigerd: %s)", r.Name, r.Reason))
		}
		ctx, cancel := context.WithCancel(s.ctx)
		s.mu.Lock()
		s.cancelAsk = cancel
		s.mu.Unlock()
		s.emit(Event{Type: "user", Text: m.Text, Files: names})
		go func() {
			defer s.running.Store(false)
			defer cancel()
			// Omzetten (PDF/OCR) kan even duren, dus ook hier: in de achtergrond en te stoppen.
			prompt, err := s.ag.withUploads(ctx, m.Text, m.Files, m.Rejected)
			if err != nil {
				if ctx.Err() != nil {
					err = fmt.Errorf("gestopt tijdens het verwerken van de bijlagen")
				}
				s.emit(Event{Type: "error", Text: err.Error()})
				s.emit(Event{Type: "done"})
				return
			}
			s.ag.Ask(ctx, prompt)
		}()
	case "confirm":
		select {
		case s.confirm <- m.OK:
		default:
		}
	case "stop":
		s.mu.Lock()
		s.cancelAsk()
		s.mu.Unlock()
		select { // een openstaande bevestiging ook afbreken
		case s.confirm <- false:
		default:
		}
	case "compact":
		if !s.running.CompareAndSwap(false, true) {
			s.emit(Event{Type: "error", Text: "nog bezig met de vorige vraag"})
			return
		}
		go func() {
			defer s.running.Store(false)
			defer s.emit(Event{Type: "done"})
			if err := s.ag.Compact(s.ctx); err != nil {
				s.emit(Event{Type: "notice", Text: "niet samengevat: " + err.Error()})
			}
			s.ag.emitContext()
		}()
	case "reset":
		if s.running.Load() {
			s.emit(Event{Type: "error", Text: "stop eerst de lopende vraag"})
			return
		}
		s.ag.Reset()
		s.mu.Lock()
		s.transcript = nil
		s.mu.Unlock()
		s.emit(Event{Type: "reset"})
		s.ag.emitContext()
	}
}

func serveWeb(addr string, cfg Config) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ongeldig adres %q (gebruik bv. 127.0.0.1:3000): %w", addr, err)
	}
	newAgent := func() *Agent { return NewAgent(cfg) }
	dbPath := filepath.Join("data", "conversations.db")
	migrateDB(filepath.Join("data", "llamatest.db"), dbPath)
	if store, err = openStore(dbPath); err != nil {
		return fmt.Errorf("database openen: %w", err)
	}
	go cleanupSessions()
	go pdfEngine() // PDFium (WebAssembly) opstarten kost ~10 s; nu al, zodat de eerste PDF snel is

	app := fiber.New(fiber.Config{AppName: "go-local-agent"})
	origins := []string{"http://localhost:" + port, "http://127.0.0.1:" + port}

	app.Get("/", func(c fiber.Ctx) error {
		c.Type("html", "utf-8")
		return c.Send(indexHTML)
	})

	// ---- gesprekkenlijst ----
	// Wijzigen mag alleen vanaf onze eigen pagina (zelfde reden als de Origin-check op de websocket).
	sameOrigin := func(c fiber.Ctx) error {
		if o := c.Get("Origin"); o != "" && !slices.Contains(origins, o) {
			return fiber.ErrForbidden
		}
		if o := c.Get("Origin"); o == "" && c.Get("Sec-Fetch-Site") != "same-origin" {
			return fiber.ErrForbidden
		}
		return c.Next()
	}
	app.Get("/api/conversations", func(c fiber.Ctx) error {
		list, err := store.list(300)
		if err != nil {
			return err
		}
		return c.JSON(list)
	})
	app.Post("/api/conversations/:id/title", sameOrigin, func(c fiber.Ctx) error {
		var body struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(c.Body(), &body); err != nil {
			return fiber.ErrBadRequest
		}
		t := strings.TrimSpace(body.Title)
		if t == "" || len([]rune(t)) > 120 {
			return fiber.ErrBadRequest
		}
		if err := store.rename(c.Params("id"), t); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Delete("/api/conversations/:id", sameOrigin, func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := store.delete(id); err != nil {
			return err
		}
		// Ook uit het geheugen halen; een tabblad dat er nog op staat, krijgt een seintje.
		sessions.Lock()
		if s, ok := sessions.m[id]; ok {
			delete(sessions.m, id)
			s.deleted.Store(true)
			s.end()
			s.mu.Lock()
			if s.conn != nil {
				s.write(Event{Type: "deleted"})
			}
			s.mu.Unlock()
		}
		sessions.Unlock()
		return c.SendStatus(fiber.StatusNoContent)
	})

	app.Use("/ws", func(c fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	// De agent kan bestanden schrijven en commando's uitvoeren, dus alleen onze eigen pagina
	// mag verbinden: zonder Origin-check kan elke website in je browser naar localhost praten.
	app.Get("/ws", websocket.New(func(c *websocket.Conn) {
		c.SetReadLimit(maxUploadMsg) // grootste toegestane bericht: bijlagen + vraag
		since, _ := strconv.Atoi(c.Query("since"))
		s, resumed := getSession(c.Query("sid"), newAgent)
		s.attach(c, since, resumed)
		defer s.detach(c)

		done := make(chan struct{})
		defer close(done)
		go func() {
			t := time.NewTicker(heartbeatEvery)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					s.mu.Lock()
					if s.conn == c {
						s.write(Event{Type: "hb"})
					}
					s.mu.Unlock()
				}
			}
		}()

		for {
			c.SetReadDeadline(time.Now().Add(readTimeout))
			var m clientMsg
			if err := c.ReadJSON(&m); err != nil {
				return
			}
			s.handle(m)
		}
	}, websocket.Config{Origins: origins}))

	log.Printf("go-local-agent web: open http://localhost:%s (model %s, context %d)", port, cfg.Model, cfg.Ctx)
	return app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true})
}
