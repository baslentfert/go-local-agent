package main

// Documenten omzetten naar tekst voor het model:
//   - PDF: per pagina eerst de echte tekst eruit halen (PDFium, als WebAssembly: pure Go, geen cgo).
//     Staat er (bijna) geen tekst op een pagina, dan is het een scan: pagina renderen en OCR doen.
//   - Afbeeldingen (png, jpg, gif, webp, bmp): OCR.
// OCR gebeurt met een OCR-model in Ollama (standaard glm-ocr), dus geen extra installatie.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

const (
	maxPDFPages   = 40   // meer pagina's lezen we niet
	maxOCRPages   = 15   // OCR kost per pagina een paar seconden; daarboven stoppen we
	minPageText   = 25   // minder tekens dan dit op een pagina = waarschijnlijk een scan
	ocrRenderSize = 1600 // langste zijde van een gerenderde pagina in pixels
)

var (
	pdfOnce sync.Once
	pdfInst pdfium.Pdfium
	pdfErr  error
	pdfMu   sync.Mutex // één document tegelijk; de WebAssembly-instantie is niet gedeeld bruikbaar
)

// pdfEngine start PDFium bij het eerste PDF-bestand (dat duurt een paar seconden).
func pdfEngine() (pdfium.Pdfium, error) {
	pdfOnce.Do(func() {
		pool, err := webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1})
		if err != nil {
			pdfErr = err
			return
		}
		pdfInst, pdfErr = pool.GetInstance(30 * time.Second)
	})
	return pdfInst, pdfErr
}

// convert maakt tekst van een binair bestand. `how` beschrijft kort wat er gebeurd is.
func (a *Agent) convert(ctx context.Context, name string, data []byte) (text, how string, err error) {
	kind := http.DetectContentType(data)
	switch {
	case kind == "application/pdf":
		return a.pdfToText(ctx, name, data)
	case strings.HasPrefix(kind, "image/"):
		a.Emit(Event{Type: "status", Text: "OCR op " + name + "…"})
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", "", fmt.Errorf("afbeelding niet te lezen (%v)", err)
		}
		text, err := a.ocr(ctx, img)
		if err != nil {
			return "", "", err
		}
		return text, "afbeelding, gelezen met OCR", nil
	}
	return "", "", fmt.Errorf("de inhoud is geen PDF of afbeelding (herkend als %s), het bestand is mogelijk beschadigd of hernoemd", kind)
}

func (a *Agent) pdfToText(ctx context.Context, name string, data []byte) (string, string, error) {
	a.Emit(Event{Type: "status", Text: "PDF openen: " + name + "…"})
	inst, err := pdfEngine()
	if err != nil {
		return "", "", fmt.Errorf("PDF-lezer start niet: %v", err)
	}
	pdfMu.Lock()
	defer pdfMu.Unlock()

	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return "", "", fmt.Errorf("PDF niet te openen, beveiligd met een wachtwoord of beschadigd (%v)", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})

	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return "", "", err
	}
	pages := count.PageCount
	if pages > maxPDFPages {
		pages = maxPDFPages
	}

	var b strings.Builder
	textPages, ocrPages, skipped := 0, 0, 0
	for i := 0; i < pages; i++ {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}}
		a.Emit(Event{Type: "status", Text: fmt.Sprintf("PDF lezen: pagina %d van %d…", i+1, pages)})

		t, err := inst.GetPageText(&requests.GetPageText{Page: page})
		pageText := ""
		if err == nil {
			pageText = strings.TrimSpace(t.Text)
		}

		if len([]rune(pageText)) < minPageText { // waarschijnlijk een scan
			if ocrPages >= maxOCRPages {
				skipped++
				continue
			}
			a.Emit(Event{Type: "status", Text: fmt.Sprintf("OCR op pagina %d van %d (scan)…", i+1, pages)})
			r, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
				Page: page, Width: ocrRenderSize, Height: ocrRenderSize,
				ImageFormat: requests.RenderImageFormatGrayscale,
			})
			if err != nil {
				return "", "", fmt.Errorf("pagina %d renderen: %v", i+1, err)
			}
			ocrText, err := a.ocr(ctx, r.Result.RenderedImage)
			r.Cleanup()
			if err != nil {
				return "", "", err
			}
			pageText = strings.TrimSpace(ocrText)
			ocrPages++
		} else {
			textPages++
		}
		if pageText != "" {
			fmt.Fprintf(&b, "--- pagina %d ---\n%s\n\n", i+1, pageText)
		}
	}

	how := fmt.Sprintf("PDF, %d pagina's", count.PageCount)
	switch {
	case ocrPages > 0 && textPages > 0:
		how += fmt.Sprintf(" (%d met tekst, %d gescand en met OCR gelezen)", textPages, ocrPages)
	case ocrPages > 0:
		how += " (gescand, met OCR gelezen)"
	}
	if count.PageCount > pages {
		how += fmt.Sprintf("; alleen de eerste %d gelezen", pages)
	}
	if skipped > 0 {
		how += fmt.Sprintf("; %d gescande pagina's overgeslagen (OCR-limiet %d)", skipped, maxOCRPages)
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", "", fmt.Errorf("geen tekst gevonden in de PDF, ook niet met OCR")
	}
	return b.String(), how, nil
}

// ocr stuurt een afbeelding naar het OCR-model in Ollama en geeft de herkende tekst terug.
func (a *Agent) ocr(ctx context.Context, img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	type ocrMsg struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images"`
	}
	body, _ := json.Marshal(map[string]any{
		"model":    a.OCRModel,
		"stream":   false,
		"messages": []ocrMsg{{Role: "user", Content: "Text Recognition:", Images: []string{base64.StdEncoding.EncodeToString(buf.Bytes())}}},
	})
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
		if strings.Contains(c.Error, "not found") {
			return "", fmt.Errorf("OCR-model %q ontbreekt: draai `ollama pull %s`", a.OCRModel, a.OCRModel)
		}
		return "", fmt.Errorf("OCR: %s", c.Error)
	}
	return c.Message.Content, nil
}
