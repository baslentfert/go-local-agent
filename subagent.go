package main

// Sub-agents, zoals Claude Code die heeft: het hoofdmodel kan werk uitbesteden met de tool
// sub_agents. Elke sub-agent is een verse Agent met een eigen, lege context en dezelfde tools
// (behalve sub_agents zelf). Hij voert één taak uit en levert een kort verslag in.
//
// Waarom: een klein model met 8k context kan geen 20 webpagina's in één gesprek lezen. Per
// taak een nieuwe context kan dat wel, en het hoofdmodel krijgt alleen de verslagen terug.
//
// Met `velden` vult elke sub-agent aan het eind een JSON-object in (Ollama dwingt het schema
// af) en maakt de code daar één tabel van, met per regel de bron.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	maxSubTasks = 25
	subMaxSteps = 8
	subReport   = 1500 // tekens per verslag die terug gaan naar het hoofdmodel
)

// Bewust zonder de werkmap erin: met "je werkt in map X" ging een sub-agent bij een webtaak
// eerst lokale bestanden bekijken.
const subPrompt = `Je bent een sub-agent: een assistent die één afgebakende taak uitvoert voor een andere assistent.
Vandaag is het {{date}} ({{os}}).

- Voer precies de gegeven taak uit, niets anders.
- Gebruik bestandstools (list_dir, read_file) alleen als de taak over bestanden gaat.
- Zoekresultaten zijn alleen fragmenten. Open bij voorkeur de eigen website van het bedrijf of product
  (het eigen domein) met fetch_url; lukt dat niet, dan een andere bron, en zeg dat het geen officiële bron is.
- Verzin niets. Kun je iets niet vinden, schrijf dan "onbekend".
- Sluit af met een kort verslag (hooguit 10 regels): wat je gevonden hebt, met de URL van elke bron.`

func (a *Agent) subSystemMessage() Message {
	cwd, _ := os.Getwd()
	return Message{Role: "system", Content: strings.NewReplacer(
		"{{cwd}}", cwd, "{{os}}", runtime.GOOS, "{{date}}", time.Now().Format("2006-01-02"),
	).Replace(subPrompt)}
}

// runSubAgents voert de taken één voor één uit (één GPU: tegelijk zou niet sneller zijn).
func (a *Agent) runSubAgents(ctx context.Context, args map[string]any) string {
	var tasks []string
	for _, l := range strings.Split(str(args, "taken"), "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*•0123456789.) "))
		if l != "" {
			tasks = append(tasks, l)
		}
	}
	if len(tasks) == 0 {
		return "fout: geen taken opgegeven (zet in 'taken' één taak per regel)"
	}
	skipped := 0
	if len(tasks) > maxSubTasks {
		skipped = len(tasks) - maxSubTasks
		tasks = tasks[:maxSubTasks]
	}
	var fields []string
	for _, f := range strings.Split(str(args, "velden"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}

	// Welk model de sub-agents draaien: gevraagd door het hoofdmodel, anders -submodel, anders hetzelfde.
	cfg := a.Config
	if cfg.SubModel != "" {
		cfg.Model = cfg.SubModel
	}
	if m := strings.TrimSpace(str(args, "model")); m != "" {
		installed, err := a.installedModels(ctx)
		if err != nil {
			return "fout: kan de lijst met modellen niet ophalen: " + err.Error()
		}
		if !slices.Contains(installed, m) && !slices.Contains(installed, m+":latest") {
			return fmt.Sprintf("fout: model %q is niet geïnstalleerd. Beschikbaar: %s. Laat 'model' leeg voor het standaardmodel.",
				m, strings.Join(installed, ", "))
		}
		cfg.Model = m
	}

	type result struct {
		report string
		values map[string]string
		err    error
	}
	results := make([]result, len(tasks))
	for i, task := range tasks {
		if ctx.Err() != nil {
			results[i].err = ctx.Err()
			continue
		}
		label := fmt.Sprintf("%d/%d", i+1, len(tasks))
		sub := &Agent{Config: cfg, Confirm: a.Confirm, sub: true}
		sub.Emit = a.subEmitter(label)
		sub.History = []Message{sub.subSystemMessage()}
		a.Emit(Event{Type: "status", Sub: label, Text: fmt.Sprintf("sub-agent %s (%s): %s", label, cfg.Model, task)})

		sub.Ask(ctx, task)
		report := lastAnswer(sub.History)
		results[i].report = report
		if len(fields) > 0 && ctx.Err() == nil {
			results[i].values, results[i].err = sub.fillFields(ctx, fields)
		}
		first := strings.SplitN(strings.TrimSpace(report), "\n", 2)[0]
		if r := []rune(first); len(r) > 120 {
			first = string(r[:120]) + "…"
		}
		a.Emit(Event{Type: "notice", Sub: label, Text: fmt.Sprintf("sub-agent %s klaar: %s", label, first)})
	}

	// Terug naar het hoofdmodel: een tabel (met velden) of de verslagen.
	var b strings.Builder
	if len(fields) > 0 {
		cols := append([]string{"taak"}, append(fields, "bron")...)
		b.WriteString("| " + strings.Join(cols, " | ") + " |\n|" + strings.Repeat("---|", len(cols)) + "\n")
		for i, r := range results {
			row := []string{cell(tasks[i])}
			for _, f := range append(fields, "bron") {
				v := "onbekend"
				if r.err != nil {
					v = "mislukt: " + r.err.Error()
				} else if r.values[f] != "" {
					v = r.values[f]
				}
				row = append(row, cell(v))
			}
			b.WriteString("| " + strings.Join(row, " | ") + " |\n")
		}
		b.WriteString("\nKorte verslagen:\n")
	}
	for i, r := range results {
		rep := r.report
		if r.err != nil && rep == "" {
			rep = "mislukt: " + r.err.Error()
		}
		if len(rep) > subReport {
			rep = rep[:subReport] + " …"
		}
		fmt.Fprintf(&b, "\n## Taak %d: %s\n%s\n", i+1, tasks[i], strings.TrimSpace(rep))
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "\n(%d taken niet uitgevoerd: maximaal %d per keer.)\n", skipped, maxSubTasks)
	}
	// Zonder deze regels liet het hoofdmodel de bronkolom weg en noemde het blogs "officiële bronnen".
	b.WriteString("\n(Neem in je antwoord de bronnen over. Noem een bron alleen 'officieel' als het de eigen " +
		"website van het bedrijf is; andere bronnen zijn reviewsites of blogs. Zeg ook wat 'onbekend' bleef. " +
		"Antwoord in de taal van de gebruiker.)\n")
	if ctx.Err() != nil {
		b.WriteString("\n(Onderbroken door de gebruiker: niet alle taken zijn afgerond.)\n")
	}
	return b.String()
}

// subEmitter laat de stappen van een sub-agent zien zonder dat zijn tekst in het hoofdantwoord
// belandt: tools en meldingen gaan door (gemarkeerd met Sub), tokens en tellers niet.
func (a *Agent) subEmitter(label string) func(Event) {
	return func(e Event) {
		switch e.Type {
		case "tool_call", "tool_result", "notice", "confirm", "confirmed":
			e.Sub = label
			a.Emit(e)
		case "status":
			e.Sub = label
			e.Text = "sub-agent " + label + ": " + e.Text
			a.Emit(e)
		case "thinking":
			// niet tonen, wel laten zien dát hij bezig is (één keer per denkblok is genoeg)
		case "error":
			a.Emit(Event{Type: "notice", Sub: label, Text: "sub-agent " + label + ": " + e.Text})
		}
	}
}

// fillFields vraagt de sub-agent zijn bevindingen in vaste velden te zetten. Ollama dwingt het
// JSON-schema af, dus er komt altijd geldige JSON terug.
func (a *Agent) fillFields(ctx context.Context, fields []string) (map[string]string, error) {
	props := map[string]any{}
	all := append(append([]string{}, fields...), "bron")
	for _, f := range all {
		props[f] = map[string]string{"type": "string"}
	}
	schema := map[string]any{"type": "object", "properties": props, "required": all}
	msgs := append(append([]Message{}, a.History...), Message{Role: "user", Content: fmt.Sprintf(
		"Zet je bevindingen nu in deze velden: %s, en 'bron' (de URL waar je het vond). Kort, feitelijk, "+
			"alleen wat je echt gevonden hebt; anders \"onbekend\". Antwoord alleen met JSON.", strings.Join(fields, ", "))})
	out, err := a.completeFormat(ctx, msgs, schema)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("geen geldige JSON terug")
	}
	vals := map[string]string{}
	for k, v := range raw {
		vals[k] = strings.TrimSpace(fmt.Sprint(v))
	}
	return vals, nil
}

// installedModels vraagt Ollama welke modellen er lokaal staan.
func (a *Agent) installedModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", a.Host+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func lastAnswer(h []Message) string {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "assistant" && strings.TrimSpace(h[i].Content) != "" {
			return h[i].Content
		}
	}
	return "(geen verslag)"
}

func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", "/")
}
