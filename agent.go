package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

type ToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Tools    []any          `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Think    bool           `json:"think"`
	Options  map[string]any `json:"options,omitempty"`
	Format   any            `json:"format,omitempty"` // JSON-schema voor gestructureerde uitvoer
}

type chatChunk struct {
	Message         Message `json:"message"`
	Done            bool    `json:"done"`
	PromptEvalCount int     `json:"prompt_eval_count"` // tokens van de hele prompt (ook bij cache)
	EvalCount       int     `json:"eval_count"`
	EvalDuration    int64   `json:"eval_duration"`
	Error           string  `json:"error"`
}

// Event is alles wat de agent naar buiten meldt; de terminal print het, de webversie
// stuurt het als JSON over de websocket.
type Event struct {
	Type   string         `json:"type"` // status, thinking, promote, token, tool_call, tool_result, confirm, stats, context, notice, compact, error, done (+ web: user, confirmed, reset, info, hb, taken)
	Seq    int            `json:"seq,omitempty"`
	Text   string         `json:"text,omitempty"`
	OK     bool           `json:"ok,omitempty"`
	SID    string         `json:"sid,omitempty"`
	Resume bool           `json:"resumed,omitempty"`
	Busy   bool           `json:"busy,omitempty"`
	Name   string         `json:"name,omitempty"`
	Sub    string         `json:"sub,omitempty"` // van welke sub-agent dit komt, bv. "3/20"
	Files  []string       `json:"files,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Tokens int            `json:"tokens,omitempty"`
	Max    int            `json:"max,omitempty"`
	TPS    float64        `json:"tps,omitempty"`
}

type Agent struct {
	Config
	History []Message
	Emit    func(Event)
	Confirm func(question string) bool

	measured    int // tokens volgens Ollama bij de laatste aanvraag (alleen de prompt)
	measuredLen int // len(History) op dat moment

	summary string   // samenvatting van wat er vóór de laatste compact gebeurde
	ledger  []string // door de code bijgehouden acties (gelezen/geschreven bestanden, commando's)
	sub     bool     // dit is een sub-agent: eigen prompt, minder stappen, geen sub_agents-tool
}

func NewAgent(cfg Config) *Agent {
	a := &Agent{Config: cfg}
	a.Reset()
	return a
}

func (a *Agent) Reset() {
	a.summary, a.ledger = "", nil
	a.History = []Message{a.systemMessage()}
	a.measured = 0
}

// Ask verwerkt één gebruikersvraag: het model aanroepen, tools uitvoeren zolang het
// model daarom vraagt, en alles als events melden. Sluit altijd af met een "done"-event.
func (a *Agent) Ask(ctx context.Context, text string) {
	defer a.Emit(Event{Type: "done"})
	a.History[0] = a.systemMessage() // prompt-bestand kan intussen aangepast zijn
	a.History = append(a.History, Message{Role: "user", Content: text})
	maxSteps := 20
	if a.sub {
		maxSteps = subMaxSteps
	}
	for step := 0; step < maxSteps; step++ {
		a.makeRoom(ctx)
		msg, err := a.chat(ctx)
		if err != nil {
			// Wat er misging hoort in de geschiedenis, anders weet het model bij de volgende
			// vraag niet dat zijn vorige antwoord half is of helemaal niet is aangekomen.
			reason := "[Onderbroken door de gebruiker]"
			if ctx.Err() != nil {
				a.Emit(Event{Type: "error", Text: "gestopt"})
			} else {
				reason = fmt.Sprintf("[Fout bij het model: %v]", err)
				a.Emit(Event{Type: "error", Text: fmt.Sprintf("%v (draait Ollama en is het model gepulld? `ollama pull %s`)", err, a.Model)})
			}
			a.History = append(a.History, Message{Role: "assistant", Content: strings.TrimSpace(msg.Content + "\n\n" + reason)})
			return
		}
		msg.Thinking = "" // denkstappen niet terugsturen, scheelt context
		a.History = append(a.History, msg)
		if len(msg.ToolCalls) == 0 {
			return
		}
		for _, tc := range msg.ToolCalls {
			a.Emit(Event{Type: "tool_call", Name: tc.Function.Name, Args: tc.Function.Arguments})
			a.note(tc)
			res := a.fitTool(a.runTool(ctx, tc))
			a.Emit(Event{Type: "tool_result", Name: tc.Function.Name, Text: res})
			a.History = append(a.History, Message{Role: "tool", ToolName: tc.Function.Name, Content: res})
		}
	}
	a.Emit(Event{Type: "error", Text: fmt.Sprintf("gestopt na %d stappen", maxSteps)})
	a.History = append(a.History, Message{Role: "assistant", Content: fmt.Sprintf("[Gestopt: na %d stappen met tools nog niet klaar]", maxSteps)})
}

// chat stuurt de geschiedenis naar Ollama, meldt het antwoord stukje voor stukje
// en geeft het volledige assistant-bericht terug.
func (a *Agent) chat(ctx context.Context) (Message, error) {
	toolDefs := tools
	if !a.sub {
		toolDefs = append(append([]any{}, tools...), subAgentTool) // sub-agents mogen zelf niet uitbesteden
	}
	body, _ := json.Marshal(chatRequest{Model: a.Model, Messages: a.History, Tools: toolDefs, Stream: true, Think: a.Think,
		Options: map[string]any{"num_ctx": a.Ctx}})
	req, err := http.NewRequestWithContext(ctx, "POST", a.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	// Sommige modellen (bv. qwen3) denken ook met think=false nog "hardop" in de content,
	// afgesloten met </think>. Zolang we dat einde niet gezien hebben, melden we de content
	// live als "thinking". Komt er geen </think>, dan was het toch het antwoord: "promote".
	var pending strings.Builder
	decided := false
	emitted := 0 // hoeveel van pending al als thinking gemeld is
	streamThinking := func(s string, final bool) {
		end := len(s)
		if !final {
			end -= len("</think>") // de tag kan half binnen zijn: dat stukje nog even vasthouden
			for end > emitted && end < len(s) && !utf8.RuneStart(s[end]) {
				end--
			}
		}
		if end > emitted {
			a.Emit(Event{Type: "thinking", Text: s[emitted:end]})
			emitted = end
		}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var c chatChunk
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			continue
		}
		if c.Error != "" {
			return out, fmt.Errorf("%s", c.Error)
		}
		if t := c.Message.Thinking; t != "" {
			a.Emit(Event{Type: "thinking", Text: t})
			out.Thinking += t
			decided = true // denken komt apart binnen, content is dus het echte antwoord
		}
		if t := c.Message.Content; t != "" {
			if decided {
				a.Emit(Event{Type: "token", Text: t})
				out.Content += t
			} else {
				pending.WriteString(t)
				s := strings.TrimPrefix(pending.String(), "<think>")
				if before, after, ok := strings.Cut(s, "</think>"); ok {
					streamThinking(before, true)
					if after = strings.TrimLeft(after, "\n"); after != "" {
						a.Emit(Event{Type: "token", Text: after})
					}
					out.Content += after
					decided = true
				} else {
					streamThinking(s, false)
				}
			}
		}
		out.ToolCalls = append(out.ToolCalls, c.Message.ToolCalls...)
		if c.Done {
			if !decided && pending.Len() > 0 {
				s := strings.TrimPrefix(pending.String(), "<think>")
				streamThinking(s, true)
				a.Emit(Event{Type: "promote"}) // geen </think> gezien: wat we toonden was het antwoord
				out.Content += s
			}
			if c.EvalDuration > 0 {
				a.Emit(Event{Type: "stats", Tokens: c.EvalCount, TPS: float64(c.EvalCount) / (float64(c.EvalDuration) / 1e9)})
			}
			// Meting van Ollama: de hele prompt zoals hij nu in History staat. Het antwoord komt er zo
			// bij, maar zonder denkstappen; dat schatten we via tokens() i.p.v. eval_count te gebruiken.
			a.measured = c.PromptEvalCount
			a.measuredLen = len(a.History)
			a.Emit(Event{Type: "context", Tokens: a.measured + charTokens([]Message{out}), Max: a.Ctx})
		}
	}
	return out, sc.Err()
}

func tool(name, desc string, params map[string]string, required ...string) any {
	props := map[string]any{}
	for k, v := range params {
		props[k] = map[string]string{"type": "string", "description": v}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  map[string]any{"type": "object", "properties": props, "required": required},
		},
	}
}

var tools = []any{
	tool("list_dir", "Toon bestanden in een map.", map[string]string{"path": "map, standaard ."}),
	tool("read_file", "Lees een tekstbestand.", map[string]string{"path": "bestandspad"}, "path"),
	tool("write_file", "Schrijf (overschrijf) een tekstbestand.", map[string]string{"path": "bestandspad", "content": "volledige inhoud"}, "path", "content"),
	tool("run_command", "Voer een shell-commando uit en geef de output terug.", map[string]string{"command": "het commando"}, "command"),
	tool("web_search", "Zoek op internet. Geeft titels, links en samenvattingen.", map[string]string{"query": "zoekopdracht"}, "query"),
	tool("fetch_url", "Haal een webpagina op en geef de leesbare tekst terug.", map[string]string{"url": "volledige URL"}, "url"),
}

// subAgentTool krijgt alleen het hoofdmodel. De beschrijving is hoe het model weet wanneer
// het deze tool moet gebruiken (net als bij Claude Code's Agent-tool).
var subAgentTool = tool("sub_agents",
	"Besteed werk uit aan sub-agents. Elke sub-agent is een verse assistent met een eigen, lege context en "+
		"dezelfde tools (zoeken, pagina's openen, bestanden lezen). Gebruik dit wanneer een vraag uit veel losse "+
		"deeltaken bestaat (bijvoorbeeld 20 bedrijven, producten of bestanden die elk apart onderzocht moeten worden), "+
		"of wanneer een deeltaak veel tekst moet lezen. Je eigen context blijft dan klein: je krijgt per taak alleen "+
		"een kort verslag terug, en met 'velden' één tabel. De taken lopen na elkaar; reken op ongeveer een halve "+
		"minuut per taak.",
	map[string]string{
		"taken":  "één taak per regel. Elke taak moet op zichzelf staan: noem de naam, wat je precies wilt weten en eventueel een URL. De sub-agent ziet dit gesprek niet.",
		"velden": "optioneel: kommagescheiden kolommen voor een vergelijkingstabel, bv. 'prijs, drawdown, consistency rule'. Elke sub-agent vult deze in, met de bron erbij.",
		"model":  "optioneel: een ander geïnstalleerd Ollama-model voor de sub-agents (bv. llama3.2:3b). Leeg = het standaardmodel.",
	}, "taken")

func str(args map[string]any, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func (a *Agent) allowed(q string) bool {
	return a.AutoYes || a.Confirm(q)
}

func (a *Agent) runTool(ctx context.Context, tc ToolCall) string {
	args := tc.Function.Arguments
	switch tc.Function.Name {
	case "list_dir":
		p := str(args, "path")
		if p == "" {
			p = "."
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return "fout: " + err.Error()
		}
		var b strings.Builder
		for _, e := range entries {
			if e.IsDir() {
				b.WriteString(e.Name() + "/\n")
			} else {
				b.WriteString(e.Name() + "\n")
			}
		}
		return b.String()
	case "read_file":
		data, err := os.ReadFile(str(args, "path"))
		if err != nil {
			return "fout: " + err.Error()
		}
		return truncate(string(data))
	case "write_file":
		p := str(args, "path")
		if !a.allowed("Schrijven naar " + p + "?") {
			return "geweigerd door gebruiker"
		}
		if dir := filepath.Dir(p); dir != "." {
			os.MkdirAll(dir, 0o755)
		}
		if err := os.WriteFile(p, []byte(str(args, "content")), 0o644); err != nil {
			return "fout: " + err.Error()
		}
		return "ok, geschreven: " + p
	case "run_command":
		c := str(args, "command")
		if !a.allowed("Commando uitvoeren: " + c + " ?") {
			return "geweigerd door gebruiker"
		}
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", c)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", c)
		}
		out, err := cmd.CombinedOutput()
		res := string(out)
		if err != nil {
			res += "\n(exit: " + err.Error() + ")"
		}
		return truncate(res)
	case "web_search":
		return webSearch(str(args, "query"))
	case "fetch_url":
		return fetchURL(str(args, "url"))
	case "sub_agents":
		if a.sub {
			return "fout: een sub-agent kan zelf geen sub-agents starten"
		}
		return a.runSubAgents(ctx, args)
	}
	return "onbekende tool: " + tc.Function.Name
}

func truncate(s string) string {
	const max = 8000
	if len(s) > max {
		return s[:max] + "\n...(afgekapt)"
	}
	return s
}
