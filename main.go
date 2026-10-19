// go-local-agent: een kleine "Claude Code"-achtige agent in Go die een lokaal Ollama-model gebruikt.
// Terminal: go run . [-model qwen3:4b] [-think]
// Web:      go run . -web 127.0.0.1:3000
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

const (
	dim   = "\033[2m"
	cyan  = "\033[36m"
	ylw   = "\033[33m"
	reset = "\033[0m"
)

func main() {
	model := flag.String("model", "qwen3:4b", "Ollama-model")
	host := flag.String("host", "http://localhost:11434", "Ollama-adres")
	think := flag.Bool("think", false, "laat het model eerst 'denken' (qwen3, deepseek-r1)")
	yes := flag.Bool("yes", false, "voer schrijf-/commando-tools uit zonder te vragen")
	web := flag.String("web", "", "start de webinterface op dit adres, bv. 127.0.0.1:3000")
	ctx := flag.Int("ctx", 8192, "contextgrootte in tokens (num_ctx); 8192 past bij qwen3:4b nog volledig op een 6 GB-GPU")
	prompt := flag.String("prompt", "prompts/system.md", "bestand met de systeemprompt (wordt bij elke vraag opnieuw gelezen)")
	ocrModel := flag.String("ocr", "glm-ocr", "Ollama-model voor OCR op gescande PDF's en afbeeldingen")
	subModel := flag.String("submodel", "", "Ollama-model voor sub-agents (bv. llama3.2:3b); leeg = hetzelfde als -model")
	flag.Parse()

	cfg := Config{Host: *host, Model: *model, Think: *think, AutoYes: *yes, Ctx: *ctx, PromptFile: *prompt, OCRModel: *ocrModel, SubModel: *subModel}
	if *web != "" {
		log.Fatal(serveWeb(*web, cfg))
	}
	runCLI(NewAgent(cfg))
}

func runCLI(ag *Agent) {
	in := bufio.NewReader(os.Stdin)
	ag.Confirm = func(q string) bool {
		fmt.Printf("%s%s [j/N] %s", ylw, q, reset)
		s, _ := in.ReadString('\n')
		s = strings.ToLower(strings.TrimSpace(s))
		return s == "j" || s == "y" || s == "ja"
	}
	ag.Emit = cliPrinter()

	fmt.Printf("%sgo-local-agent%s — model %s, context %d tokens. Typ je vraag; /compact, /reset, /exit.\n", cyan, reset, ag.Model, ag.Ctx)
	for {
		fmt.Print(cyan + "\n> " + reset)
		line, err := in.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return
		case "/reset":
			ag.Reset()
			fmt.Println("geschiedenis gewist")
			continue
		case "/compact":
			if err := ag.Compact(context.Background()); err != nil {
				fmt.Println("niet samengevat:", err)
			}
			continue
		}
		ag.Ask(context.Background(), line)
	}
}

// cliPrinter zet agent-events om naar gekleurde terminaluitvoer.
func cliPrinter() func(Event) {
	mode := "" // "status", "thinking" of "token": wat er nu op het scherm loopt
	switchTo := func(m string) {
		switch {
		case mode == "status":
			fmt.Print("\r\033[K")
		case mode == "thinking" && m != "thinking":
			fmt.Print(reset + "\n\n")
		}
		if m == "thinking" && mode != "thinking" {
			fmt.Print(dim)
		}
		mode = m
	}
	return func(e Event) {
		switch e.Type {
		case "status":
			switchTo("status")
			fmt.Print(dim + "(" + e.Text + ")" + reset)
		case "thinking":
			switchTo("thinking")
			fmt.Print(e.Text)
		case "token":
			switchTo("token")
			fmt.Print(e.Text)
		case "promote":
			// het "denkwerk" bleek het antwoord; staat al op het scherm, alleen de kleur klopt niet
			switchTo("")
		case "stats":
			switchTo("")
			fmt.Printf("\n%s[%d tokens, %.1f tok/s]%s\n", dim, e.Tokens, e.TPS, reset)
		case "tool_call":
			switchTo("")
			args, _ := json.Marshal(e.Args)
			fmt.Printf("%s⚙ %s %s%s\n", ylw, e.Name, args, reset)
		case "tool_result":
			fmt.Printf("%s%s%s\n", dim, firstLines(e.Text, 6), reset)
		case "context":
			// stil in de terminal; de meter staat in de webversie
		case "notice":
			switchTo("")
			fmt.Printf("%s· %s%s\n", dim, e.Text, reset)
		case "compact":
			switchTo("")
			fmt.Printf("%s· gesprek samengevat (%d → %d tokens):\n%s%s\n\n", dim, e.Tokens, e.Max, e.Text, reset)
		case "error":
			switchTo("")
			fmt.Printf("\nfout: %s\n", e.Text)
		case "done":
			switchTo("")
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (+%d regels)", len(lines)-n)
	}
	return strings.Join(lines, "\n")
}
