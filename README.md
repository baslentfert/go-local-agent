# go-local-agent

A small AI agent in Go that runs the model on your own machine through
[Ollama](https://ollama.com). It works in the style of Claude Code: the model can
read and write files, run commands, search the web and delegate work to
sub-agents, while the app manages the context window for it.

It is a learning and experimentation project: a single Go binary, no API keys or
paid services, and small enough to read in an afternoon.

**What stays local and what doesn't:** the model, your conversations and your
uploaded files never leave your machine. The web tools do go online: web search
queries go to DuckDuckGo, and `fetch_url` downloads the pages the model asks for.

> The user interface, the default prompts and the code comments are in **Dutch**.

## Features

- **Agent loop with tools**: list/read/write files, run shell commands
  (PowerShell on Windows), web search (DuckDuckGo, ads filtered out) and
  fetching pages. Writing files and running commands ask for confirmation.
- **Terminal and web UI**. The web UI (Fiber v3 + WebSocket) streams tokens and
  the model's thinking live, renders Markdown safely (model output is always
  escaped first), and reconnects automatically with replay of missed events.
- **Context management like Claude Code**: a context meter, trimming of old tool
  results, automatic and manual summarizing (`/compact`), and an action list
  kept by the code itself so facts survive a summary.
- **System prompt from a file** (`prompts/system.md`), re-read on every question,
  so you can tune prompts without restarting.
- **Attachments**: text files, PDFs (text extracted with PDFium compiled to
  WebAssembly, no cgo) and images / scanned pages via an OCR model in Ollama
  (`glm-ocr`). Every attachment reaches the model with its status, including
  failures.
- **Conversation history** in SQLite (pure Go driver): a sidebar to reopen,
  rename, search and delete conversations.
- **Slash commands** with a picker: type `/`, choose with the arrow keys, Enter
  runs, Tab completes, Esc closes.

  | Command | Meaning |
  |---|---|
  | `/nieuw` (also `/new`, `/reset`, `/clear`) | new conversation (the old one is kept) |
  | `/compact` | summarize the conversation to free up context |
  | `/gesprekken` (`/lijst`) | show or hide the conversation list |
  | `/hernoem` (`/rename`) | rename this conversation |
  | `/verwijder` (`/delete`) | delete this conversation |
  | `/bijlage` (`/upload`) | attach a file |
- **Sub-agents**: the main model can hand off many separate tasks (for example
  "compare 20 companies") to fresh agents with their own context, optionally on
  a different model, and get back short reports or one comparison table with a
  source per row.

## Requirements

- [Go](https://go.dev) 1.27 or newer (only to build; no C compiler needed)
- A recent [Ollama](https://ollama.com), running locally (tool calling and the
  `think` option are used)
- A chat model with tool calling, for example:
  ```
  ollama pull qwen3:4b
  ```
- Optional, for OCR on images and scanned PDFs:
  ```
  ollama pull glm-ocr
  ```

## Build and run

Run it **from the project folder**: the system prompt (`prompts/system.md`) and
the `data/` and `uploads/` folders are found relative to the current directory.

```
git clone https://github.com/baslentfert/go-local-agent
cd go-local-agent

# Windows
go build -o go-local-agent.exe .
.\go-local-agent.exe -web 127.0.0.1:3000

# Linux / macOS
go build -o go-local-agent .
./go-local-agent -web 127.0.0.1:3000
```

Then open <http://localhost:3000>.

**Terminal mode:** start it without `-web`. Type a question and press Enter;
`/compact` summarizes, `/reset` starts over, `/exit` quits. Attachments and the
conversation list are only available in the web UI.

**First run:** the PDF engine needs about 10 seconds to start (the web server
does this in the background), and the first OCR request waits until Ollama has
loaded the OCR model. After that, an OCR page takes a couple of seconds.

| Flag | Default | Meaning |
|---|---|---|
| `-model` | `qwen3:4b` | Ollama model for the agent |
| `-ctx` | `8192` | Context size (`num_ctx`). 8k fits `qwen3:4b` fully on a 6 GB GPU; larger spills to the CPU and gets slower. |
| `-prompt` | `prompts/system.md` | System prompt file. Variables: `{{model}}`, `{{cwd}}`, `{{os}}`, `{{date}}`. `<!-- comments -->` are stripped. |
| `-web` | (off) | Serve the web UI on this address, for example `127.0.0.1:3000` |
| `-submodel` | same as `-model` | Model for sub-agents, for example `llama3.2:3b` |
| `-ocr` | `glm-ocr` | Model for OCR |
| `-think` | `false` | Let models that support it think before answering |
| `-yes` | `false` | Run write and command tools without asking (careful) |
| `-host` | `http://localhost:11434` | Ollama address |

Runtime data is written to the current directory: `data/conversations.db` holds
the conversations and `uploads/` the attached files and their extracted text.
Both are git-ignored.

## Security

The agent can **write files and run commands on your machine**.

- Bind the web UI to `127.0.0.1` (as above). Do not expose it on a network.
- The WebSocket and the conversation API only accept requests from the page's
  own origin, so other websites in your browser cannot drive the agent.
- Without `-yes`, every file write and command needs your confirmation.
- Text from web pages and uploads can contain instructions aimed at the model
  (prompt injection). Keep confirmations on when you let it browse.

## Limitations

- Small local models make mistakes: they misread sources, overthink, and
  sometimes present search snippets as verified facts. The prompts and tool
  results push back on this, but check important answers yourself.
- Speed depends on your GPU. On an RTX 3050 (6 GB) `qwen3:4b` runs at about
  45 tokens/s; a sub-agent that searches and reads pages takes a few minutes per
  task.
- PDFs: at most 40 pages per file, at most 15 of them through OCR.
- Office files (Word, Excel) are not supported.
- Web search scrapes DuckDuckGo's HTML page and can break if that page changes.

## Project layout

| File | What it does |
|---|---|
| `main.go` | Flags, terminal mode |
| `agent.go` | Agent loop, streaming from Ollama, tool definitions and execution |
| `context.go` | System prompt, context meter, trimming and summarizing, action list |
| `subagent.go` | Sub-agents and the comparison table |
| `server.go` | Web server, WebSocket sessions, reconnect/replay, conversation API |
| `store.go` | SQLite storage of conversations |
| `upload.go` | Attachments: saving, status per file, inlining into the question |
| `docs.go` | PDF text extraction and OCR |
| `web.go` | Web search and page fetching |
| `web/index.html` | The whole web UI (embedded in the binary) |
| `prompts/system.md` | Default system prompt |

## Credits

Built on [Ollama](https://ollama.com), [Fiber](https://gofiber.io),
[go-pdfium](https://github.com/klippa-app/go-pdfium) (PDFium via WebAssembly),
[modernc.org/sqlite](https://modernc.org/sqlite) and
[GLM-OCR](https://ollama.com/library/glm-ocr).

## License

[MIT](LICENSE)
