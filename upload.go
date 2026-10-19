package main

// Bestanden die je in de webpagina toevoegt (knop, slepen of plakken):
//   - tekstbestanden gaan direct mee
//   - PDF's en afbeeldingen worden eerst omgezet naar tekst (zie docs.go: PDF-tekst of OCR)
//
// Belangrijk: wat er met elke bijlage gebeurd is (gelukt, en hoe; of mislukt, en waarom) gaat
// altijd mee naar het model. Anders weet het model niet dat er iets geüpload is, of denkt het
// dat het gelukt is terwijl dat niet zo is. Eén mislukte bijlage houdt de rest niet tegen.
//
// Elk bestand wordt ook opgeslagen in uploads/ (bij PDF/afbeelding ook de omgezette tekst als
// .txt), zodat het model het later met read_file opnieuw kan lezen.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

type upload struct {
	Name    string `json:"name"`
	Content string `json:"content,omitempty"` // tekstbestand
	Data    string `json:"data,omitempty"`    // binair bestand (PDF/afbeelding), base64
}

// rejectedFile is een bestand dat de pagina zelf al geweigerd heeft (rood label).
type rejectedFile struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

const (
	uploadDir     = "uploads"
	maxUploads    = 10
	maxUploadSize = 512 * 1024       // tekstbestand; de pagina controleert dit ook al
	maxBinarySize = 20 * 1024 * 1024 // PDF of afbeelding
	maxUploadMsg  = 45 * 1024 * 1024 // grootste websocketbericht (base64 is ~4/3 groter)
	inlineShare   = 0.4              // bijlagen samen hooguit dit deel van de context in de vraag zelf
)

var unsafeName = regexp.MustCompile(`[^\w.\-]+`)

func safeName(n string) string {
	n = unsafeName.ReplaceAllString(filepath.Base(n), "_") // geen ../ of rare tekens
	if n == "" || n == "." || n == ".." {
		n = "bestand"
	}
	return n
}

// saveUpload slaat één bijlage op en geeft de tekst terug, het pad van die tekst en hoe hij
// verkregen is.
func (a *Agent) saveUpload(ctx context.Context, f upload) (content, path, how string, err error) {
	name := safeName(f.Name)
	path = filepath.Join(uploadDir, name)

	if f.Data == "" { // tekstbestand
		if len(f.Content) > maxUploadSize {
			return "", "", "", fmt.Errorf("te groot (max %d KB)", maxUploadSize/1024)
		}
		if !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) {
			return "", "", "", fmt.Errorf("geen tekstbestand")
		}
		return f.Content, path, "tekstbestand", os.WriteFile(path, []byte(f.Content), 0o644)
	}

	// PDF of afbeelding: origineel bewaren en omzetten naar tekst
	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		return "", "", "", fmt.Errorf("upload is beschadigd aangekomen")
	}
	if len(data) > maxBinarySize {
		return "", "", "", fmt.Errorf("te groot (max %d MB)", maxBinarySize/1024/1024)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", "", "", err
	}
	if content, how, err = a.convert(ctx, name, data); err != nil {
		return "", "", "", err
	}
	path += ".txt" // de omgezette tekst ernaast, zodat read_file hem kan lezen
	return content, path, how, os.WriteFile(path, []byte(content), 0o644)
}

// withUploads zet de bijlagen om, slaat ze op en zet ze — met per bestand de status — vóór de vraag.
// Geeft alleen een fout terug als de gebruiker stopt; mislukte bijlagen komen in de prompt zelf.
func (a *Agent) withUploads(ctx context.Context, text string, files []upload, rejected []rejectedFile) (string, error) {
	if len(files) == 0 && len(rejected) == 0 {
		return text, nil
	}
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return "", err
	}
	if len(files) > maxUploads {
		for _, f := range files[maxUploads:] {
			rejected = append(rejected, rejectedFile{f.Name, fmt.Sprintf("meer dan %d bestanden tegelijk", maxUploads)})
		}
		files = files[:maxUploads]
	}

	var b strings.Builder
	ok, failed := 0, 0
	fail := func(name, reason string) {
		failed++
		a.ledger = append(a.ledger, fmt.Sprintf("upload MISLUKT: %s (%s)", name, reason))
		a.Emit(Event{Type: "notice", Text: fmt.Sprintf("%s: niet gelukt — %s", name, reason)})
		fmt.Fprintf(&b, "<bestand naam=%q status=\"mislukt\">\nDit bestand is NIET gelukt: %s. De inhoud is dus niet beschikbaar.\n</bestand>\n\n", name, reason)
	}

	for _, r := range rejected {
		fail(r.Name, "geweigerd door de pagina: "+r.Reason)
	}

	// Hoeveel tekens er per bestand in de vraag zelf passen.
	budget := int(float64(a.Ctx)*inlineShare*charsPerToken) / max(1, len(files))

	for _, f := range files {
		content, path, how, err := a.saveUpload(ctx, f)
		if ctx.Err() != nil {
			a.ledger = append(a.ledger, "uploads afgebroken door de gebruiker")
			return "", ctx.Err()
		}
		if err != nil {
			fail(f.Name, err.Error())
			continue
		}
		ok++
		p := filepath.ToSlash(path)
		a.ledger = append(a.ledger, fmt.Sprintf("upload gelukt: %s → %s (%s)", f.Name, p, how))
		if how != "tekstbestand" {
			a.Emit(Event{Type: "notice", Text: f.Name + ": " + how})
		}

		shown, note := content, ""
		if len(shown) > budget {
			cut := budget
			for cut > 0 && !utf8.RuneStart(shown[cut]) {
				cut--
			}
			shown = shown[:cut]
			note = fmt.Sprintf("\n…[alleen het begin getoond: %d van %d tekens. Lees verder met read_file op %s]", cut, len(content), p)
		}
		warn := ""
		if strings.Contains(how, "OCR") {
			warn = "\n(Let op: deze tekst is met OCR uit een afbeelding/scan gehaald en kan leesfouten bevatten.)"
		}
		fmt.Fprintf(&b, "<bestand naam=%q status=\"gelukt\" bron=%q pad=%q tekens=\"%d\">\n%s%s%s\n</bestand>\n\n",
			f.Name, how, p, len(content), shown, note, warn)
	}

	switch {
	case ok > 0 && failed > 0:
		b.WriteString("(Niet alle bijlagen zijn gelukt: zie status=\"mislukt\" hierboven. Zeg dat eerlijk als de vraag erover gaat. De gelukte bijlagen staan hierboven; die hoef je niet opnieuw te lezen.)\n\n")
	case ok > 0:
		b.WriteString("(De inhoud van de bijlage staat hierboven; je hoeft hem niet opnieuw te lezen.)\n\n")
	default:
		b.WriteString("(Geen enkele bijlage is gelukt, zie hierboven. Je hebt de inhoud dus niet; zeg dat eerlijk en verzin niets.)\n\n")
	}
	if strings.TrimSpace(text) == "" {
		text = "Bekijk het bijgevoegde bestand en vertel kort wat erin staat."
	}
	return b.String() + text, nil
}
