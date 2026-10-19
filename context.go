package main

// Contextbeheer, zoals Claude Code het ook doet:
//   1. systeemprompt uit een bestand, bij elke vraag opnieuw ingelezen (prompts/system.md)
//   2. weten hoe vol de context is (Ollama meldt prompt_eval_count; daartussen schatten we)
//   3. onder druk eerst oude toolresultaten inkorten ("microcompact")
//   4. helpt dat niet genoeg: het gesprek tot nu toe laten samenvatten ("compact")

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

type Config struct {
	Host       string
	Model      string
	Think      bool
	AutoYes    bool
	Ctx        int    // num_ctx: hoeveel tokens het model in één keer ziet
	PromptFile string // systeemprompt; leeg of niet gevonden = ingebouwde standaard
	OCRModel   string // Ollama-model voor OCR op scans en afbeeldingen
	SubModel   string // Ollama-model voor sub-agents; leeg = hetzelfde als Model
}

const (
	compactAt      = 0.75 // vanaf dit deel van de context gaan we ruimte maken
	charsPerToken  = 3.5  // grove schatting voor Nederlands/Engels; gemeten ~3,6
	toolsOverhead  = 700  // tokens voor de tooldefinities die elke aanvraag meegaan
	keepRecentMsgs = 6    // toolresultaten in de laatste zoveel berichten blijven heel
	trimToolTo     = 300  // oudere toolresultaten worden ingekort tot zoveel tekens
)

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

const defaultPrompt = `Je bent een behulpzame programmeerassistent die lokaal draait (model {{model}}).
Je werkt in map {{cwd}} op {{os}}. Vandaag is het {{date}}.

Gebruik de tools om bestanden te bekijken, te schrijven en commando's uit te voeren wanneer dat helpt.
Bij vragen over actuele feiten (versies, nieuws, prijzen): gebruik web_search en open daarna de meest
officiële bron met fetch_url voordat je antwoordt; vertrouw niet op losse zoeksnippets.

Antwoord in de taal van de gebruiker, kort en concreet. Gebruik markdown waar dat helpt.`

// systemMessage leest de systeemprompt (elke keer opnieuw, zodat je hem tijdens het
// gebruik kunt aanpassen) en vult de variabelen in.
func (a *Agent) systemMessage() Message {
	if a.sub {
		return a.subSystemMessage()
	}
	text := defaultPrompt
	if a.PromptFile != "" {
		if b, err := os.ReadFile(a.PromptFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			text = string(b)
		}
	}
	text = strings.TrimSpace(htmlComment.ReplaceAllString(text, "")) // <!-- notities --> zijn voor jou, niet voor het model
	cwd, _ := os.Getwd()
	text = strings.NewReplacer(
		"{{cwd}}", cwd,
		"{{os}}", runtime.GOOS,
		"{{date}}", time.Now().Format("2006-01-02"),
		"{{model}}", a.Model,
	).Replace(text)

	// Na een compact: wat er eerder gebeurde. In de systeemprompt, niet als nep-gebruikersbericht:
	// kleine modellen gaan anders denken dat de gebruiker die samenvatting zelf stuurde.
	if a.summary != "" {
		text += "\n\n# Eerder in dit gesprek (samenvatting)\n" + a.summary
	}
	if len(a.ledger) > 0 {
		text += "\n\n# Acties in dit gesprek (automatisch bijgehouden, betrouwbaar)\n- " + strings.Join(a.ledger, "\n- ")
	}
	return Message{Role: "system", Content: text}
}

const maxLedger = 40

// note houdt bij wat de agent gedaan heeft. Dit doet de code, niet het model, zodat het
// een samenvatting overleeft, ook als een klein model iets vergeet.
func (a *Agent) note(tc ToolCall) {
	args := tc.Function.Arguments
	var entry string
	switch tc.Function.Name {
	case "read_file":
		entry = "gelezen: " + str(args, "path")
	case "write_file":
		entry = "geschreven: " + str(args, "path")
	case "list_dir":
		p := str(args, "path")
		if p == "" {
			p = "."
		}
		entry = "map bekeken: " + p
	case "run_command":
		entry = "commando: " + str(args, "command")
	case "web_search":
		entry = "gezocht: " + str(args, "query")
	case "fetch_url":
		entry = "pagina geopend: " + str(args, "url")
	case "sub_agents":
		n := len(strings.FieldsFunc(str(args, "taken"), func(r rune) bool { return r == '\n' }))
		entry = fmt.Sprintf("sub-agents ingezet: %d taken", n)
		if v := str(args, "velden"); v != "" {
			entry += " (velden: " + v + ")"
		}
	default:
		return
	}
	for i, e := range a.ledger { // dubbele weghalen, de nieuwste komt achteraan
		if e == entry {
			a.ledger = append(a.ledger[:i], a.ledger[i+1:]...)
			break
		}
	}
	a.ledger = append(a.ledger, entry)
	if len(a.ledger) > maxLedger {
		a.ledger = a.ledger[len(a.ledger)-maxLedger:]
	}
}

func charTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + 20 // + rol/opmaak per bericht
		for _, tc := range m.ToolCalls {
			b, _ := json.Marshal(tc)
			n += len(b)
		}
	}
	return int(float64(n) / charsPerToken)
}

// tokens schat hoe groot de volgende aanvraag wordt: de laatst gemeten waarde van Ollama
// plus een schatting voor wat er sindsdien bij is gekomen.
func (a *Agent) tokens() int {
	if a.measured > 0 && a.measuredLen <= len(a.History) {
		return a.measured + charTokens(a.History[a.measuredLen:])
	}
	return toolsOverhead + charTokens(a.History)
}

func (a *Agent) emitContext() {
	a.Emit(Event{Type: "context", Tokens: a.tokens(), Max: a.Ctx})
}

// fitTool zorgt dat één toolresultaat nooit meer dan ~30% van de context inneemt.
// Bij een kleine context (bv. 4k) is de vaste grens van 8000 tekens anders al bijna alles.
func (a *Agent) fitTool(s string) string {
	max := int(float64(a.Ctx) * 0.3 * charsPerToken)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[afgekapt: %d van %d tekens getoond, de context is te klein voor meer]", cut, len(s))
}

// trimToolResults kort toolresultaten in, behalve die in de laatste `keep` berichten.
// Geeft terug hoeveel er zijn ingekort.
func (a *Agent) trimToolResults(keep int) int {
	n := 0
	for i := 1; i < len(a.History)-keep; i++ {
		m := &a.History[i]
		if m.Role != "tool" || len(m.Content) <= trimToolTo+100 {
			continue
		}
		m.Content = m.Content[:trimToolTo] + fmt.Sprintf("\n…[ingekort om ruimte te sparen; origineel %d tekens. Roep de tool opnieuw aan als je het nodig hebt]", len(m.Content))
		n++
	}
	if n > 0 {
		a.measured = 0 // geschiedenis veranderd: meting klopt niet meer
	}
	return n
}

// makeRoom zorgt dat de volgende aanvraag in de context past.
func (a *Agent) makeRoom(ctx context.Context) {
	limit := int(float64(a.Ctx) * compactAt)
	if a.tokens() <= limit {
		return
	}
	if n := a.trimToolResults(keepRecentMsgs); n > 0 {
		a.Emit(Event{Type: "notice", Text: fmt.Sprintf("%d oude toolresultaten ingekort", n)})
		if a.tokens() <= limit {
			a.emitContext()
			return
		}
	}
	if err := a.Compact(ctx); err != nil {
		// samenvatten lukte niet (of er valt niets samen te vatten): dan alles inkorten
		if n := a.trimToolResults(0); n > 0 {
			a.Emit(Event{Type: "notice", Text: fmt.Sprintf("samenvatten lukte niet (%v); %d toolresultaten ingekort", err, n)})
		}
	}
	a.emitContext()
}

const compactPrompt = `Je werkt de samenvatting bij van een gesprek tussen een gebruiker en een programmeerassistent,
zodat de assistent daarna zonder de volledige geschiedenis verder kan.

Je krijgt eventueel een EERDERE SAMENVATTING en daarna het NIEUWE GESPREK. Maak één samenvatting van alles:
neem de eerdere samenvatting volledig over en vul aan met het nieuwe gesprek. Laat niets weg wat nog relevant is.

Schrijf in het Nederlands, beknopt maar volledig, met deze kopjes:
- Vragen van de gebruiker: alle vragen tot nu toe, in volgorde, met kort het antwoord.
- Belangrijke details: bestandsnamen, paden, waarden, foutmeldingen, besluiten.
- Open: wat moet er nog gebeuren?

Geef alleen de samenvatting, zonder inleiding.`

// Compact vat alles vóór de laatste gebruikersvraag samen (samen met een eventuele eerdere
// samenvatting) en haalt het uit de geschiedenis. De lopende vraag blijft letterlijk staan.
func (a *Agent) Compact(ctx context.Context) error {
	split := 0
	for i := len(a.History) - 1; i >= 1; i-- {
		if a.History[i].Role == "user" {
			split = i
			break
		}
	}
	if split == 0 { // nog geen vraag gesteld: alles samenvatten
		split = len(a.History)
	}
	old := a.History[1:split]
	if len(old) < 2 {
		return fmt.Errorf("te weinig om samen te vatten")
	}

	var t strings.Builder
	if a.summary != "" {
		fmt.Fprintf(&t, "EERDERE SAMENVATTING:\n%s\n\nNIEUW GESPREK:\n\n", a.summary)
	}
	for _, m := range old {
		c := m.Content
		if len(c) > 1500 {
			c = c[:1500] + " …"
		}
		switch m.Role {
		case "user":
			fmt.Fprintf(&t, "GEBRUIKER: %s\n\n", c)
		case "assistant":
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Function.Arguments)
				fmt.Fprintf(&t, "ASSISTENT roept %s aan met %s\n", tc.Function.Name, args)
			}
			if c != "" {
				fmt.Fprintf(&t, "ASSISTENT: %s\n\n", c)
			}
		case "tool":
			fmt.Fprintf(&t, "RESULTAAT %s: %s\n\n", m.ToolName, c)
		}
	}

	a.Emit(Event{Type: "status", Text: "gesprek samenvatten…"})
	summary, err := a.complete(ctx, []Message{
		{Role: "system", Content: compactPrompt},
		{Role: "user", Content: t.String()},
	})
	if err != nil {
		return err
	}
	if _, after, ok := strings.Cut(summary, "</think>"); ok { // qwen3 denkt soms toch hardop
		summary = after
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return fmt.Errorf("lege samenvatting")
	}

	before := a.tokens()
	a.summary = summary
	rest := append([]Message(nil), a.History[split:]...)
	a.History = append([]Message{a.systemMessage()}, rest...)
	a.measured = 0
	a.Emit(Event{Type: "compact", Text: summary, Tokens: before, Max: a.tokens()})
	return nil
}

// complete doet één aanvraag zonder tools en zonder streaming (voor de samenvatting).
func (a *Agent) complete(ctx context.Context, msgs []Message) (string, error) {
	return a.completeFormat(ctx, msgs, nil)
}

// completeFormat is complete met een optioneel JSON-schema: Ollama laat het model dan alleen
// tekst maken die aan het schema voldoet.
func (a *Agent) completeFormat(ctx context.Context, msgs []Message, schema any) (string, error) {
	body, _ := json.Marshal(chatRequest{Model: a.Model, Messages: msgs, Stream: false, Think: false,
		Options: map[string]any{"num_ctx": a.Ctx}, Format: schema})
	req, err := http.NewRequestWithContext(ctx, "POST", a.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var c chatChunk
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", err
	}
	if c.Error != "" {
		return "", fmt.Errorf("%s", c.Error)
	}
	return c.Message.Content, nil
}
