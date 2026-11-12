# file.read for images and documents (plan, 2026-11-12)

## Why

Today Alfred can only read text files in the chat folder. A screenshot, a PDF, a `.docx` or an `.xlsx` is refused as "not text (binary file)". The owner wants Alfred to read these single files in the chat folder too.

Rule 1 still holds. Alfred reads on the kernel's own loop, and runtime tools stay off.

## Decisions (agreed with the owner, 2026-11-12)

1. **Images are multimodal input on every adapter.** The model actually sees the picture: no OCR, no metadata-only description.
2. **Image bytes live in the event log, downscaled.**
   - Images are decoded with the stdlib (`image/png`, `image/jpeg`, `image/gif`).
   - Each one is shrunk until its long edge is at most 1568 px, then re-encoded.
   - At most 1 MB per image and one image per call.
   - Replay is deterministic, so the cache stays stable.
   - webp and heic are refused, so no new dependency is needed.
3. **Documents are extracted to text with the stdlib, plus `pdftotext` for PDF.**
   - `docx`, `xlsx` and `pptx` are zip+XML, read with `archive/zip` and `encoding/xml`.
   - PDF goes through the `pdftotext` binary: fixed argv, no shell, path from config, the same way `rg` works.
   - Old binary `xls` is refused with "save it as xlsx".
4. **`file.read` is extended; there is no new tool.** It picks a reader by extension, like Claude Code's Read.
5. **Non-UTF-8 text:**
   - A file is refused as binary only when its first 8 KiB contain a NUL byte, the same heuristic git uses.
   - Otherwise, invalid bytes in the lines a read returns are replaced with U+FFFD. Lines outside the range are not checked.

Trust needed no decision. Rule 4 applies: image and document content is always `Untrusted`, so reading one taints the task.

## Core type change (core/task)

```go
type Image struct {
	MediaType string `json:"media_type"` // image/png | image/jpeg
	Data      []byte `json:"data"`       // base64 in JSON
}

type ToolResult struct {
	// … existing fields
	Images []Image `json:"images,omitempty"`
}
```

- `Content` still carries a short text line, so text-only paths and logs have something: `image photo.png, 1568×1176 (from 4032×3024), 412 KiB`.
- `app.capResult` (40 KiB) applies to `Content` only. Images have their own 1 MB cap, which is enforced when the image is produced.
- `appendResult` and the conversation replay copy `Images` through unchanged.

## file.read dispatch (adapters/tools/local/readfile)

Selection is by lower-cased extension, after the existing resolve, deny-list check, `os.Root` open and `SameFile` check. Every path reads through the same opened `*os.File`.

| Extension | Reader | Result |
| --- | --- | --- |
| `.png .jpg .jpeg .gif` | `image.go` | `Images` + summary line; offset/limit ignored |
| `.pdf` | `pdftotext -q -layout -enc UTF-8 - -`, file bytes on stdin | text → `numberedLines` |
| `.docx` | `word/document.xml`: paragraphs as lines, table cells joined by tab | text → `numberedLines` |
| `.xlsx` | sheet names + shared strings; each sheet `# sheet: <name>`, then rows as tab-separated cells placed by column ref | text → `numberedLines` |
| `.pptx` | `ppt/slides/slideN.xml` in slide order, `# slide N`, then `a:t` text | text → `numberedLines` |
| `.xls .doc .ppt` | refuse | "old binary Office format; save it as .xlsx/.docx/.pptx" |
| `.webp .heic .bmp .tiff .svg` | refuse | "image format not supported; convert to png or jpeg" |
| anything else | existing text path (new UTF-8 rule) | unchanged |

Common rules for documents:
- Extracted text goes through the same `numberedLines` code: offset/limit, `N|line`, 2000-char lines, 32 KiB output, and `continue with offset=N`.
- The "256 KiB needs a range" rule applies to the **extracted text**, not to the file size.
- Raw file limit: 50 MB for documents and 20 MB for images.
- Zip bomb guard: each zip member is read through `io.LimitReader` with a budget of 64 MB decompressed for the whole document.
- Image decode bomb guard: `image.DecodeConfig` runs first, and anything over 40 megapixels is refused before a full decode.
- PDF:
  - 30 s timeout and a 16 MB stdout cap.
  - Form feeds (`\f`) become a `# page N` line.
  - Empty output becomes "no text layer (scanned PDF?)".
  - If `pdftotext` is missing, the tool says to install poppler.
- `xlsx` dates and numbers are written as their stored raw value. Styles are not applied, so a date shows as a serial number like `45972`. *(Claude's call: keeps the reader small; revisit if it bothers you.)*
- `file.search` (rg) still does not search inside documents. That is noted in the description.

Image path:
- Decode, then downscale with a small stdlib box filter (no `x/image`).
- Re-encode:
  - The output is PNG if the source is PNG or GIF (keeps text in screenshots crisp), and JPEG q85 if the source is JPEG.
  - If the PNG is over 1 MB, re-encode it as JPEG q85, then q70. If it is still over 1 MB, refuse.
- A GIF uses its first frame only.

New `Config` field: `Pdftotext string`, which defaults to `pdftotext` on PATH, like `Rg`. `readfile.Run` gets a stdin parameter, or a sibling helper does. That choice is made while implementing.

The `file.read` description becomes: "Read a file in the chat folder. Text files, PDF, docx, xlsx and pptx come back as numbered lines (use offset/limit); png, jpeg and gif come back as an image."

## Model adapters

| Adapter | Rendering of a `ToolResult` with `Images` |
| --- | --- |
| anthropic | `tool_result.content` becomes an array: `[{type:text}, {type:image, source:{type:base64, media_type, data}}]` |
| chatgpt (Responses) | `function_call_output.output` becomes an array: `[{type:input_text}, {type:input_image, image_url:"data:<mt>;base64,…"}]` |
| openai (chat completions) | the `tool` message stays text; one `user` message right after the tool messages carries `image_url` data URIs, with text `"images from tool call <id>"` |
| claudesdk (`claude -p`) | see below |

**claudesdk.** The prompt is still rendered as text, with a placeholder `[image <n> from tool call <id>]` wherever an image appears. That keeps the `--resume` prefix diff working. `StepRequest` gains `Images []task.Image`, in placeholder order. `CLI.run` then works like this:
- If the stdin delta it sends has no placeholder, nothing changes: plain text stdin and `--output-format json`.
- Otherwise it sends one stream-json user message whose content alternates text and `image` blocks, split at the placeholders. It runs with `--input-format stream-json --output-format stream-json --verbose` and reads the final `result` line, which has the same fields as `cliResult`.

## Uncertain (verify live before the PR is marked ready)

- `// TODO: uncertain`: does the ChatGPT backend (`chatgpt.com/backend-api/codex/responses`) accept an array `output` on `function_call_output`?
  - The public Responses API does, and recent Codex sends `input_image` there.
  - If the backend rejects it, fall back to the openai approach: a user message after the outputs.
- **Verified live (2026-11-12):** `claude -p` with `--input-format stream-json`, `--json-schema` and `--resume` returns `structured_output` on the `result` line. The model named a red test square, and a text-only resume still saw it.
- Token cost: a 1568 px image is about 1.6k tokens on Claude, and it is replayed on every later step of the conversation. Accepted for now. Compaction at 92% still bounds it.

## Not in scope

- OCR for scanned PDFs, sending PDFs natively as documents, and webp/heic.
- Old `xls`/`doc`/`ppt`, `odt`/`ods`, and images embedded inside docx or pptx.
- Searching inside documents.
- Images from channels (Discord attachments, a TUI paste). This plan covers chat-folder files only, but `task.Image` is shaped so that a user message could carry images later.

## Steps (TDD, one PR)

1. `core/task`: add the `Image` type and the `ToolResult.Images` field, then a JSON round-trip test.
2. `app`: make sure `capResult`, `appendResult` and the replay keep `Images`. A test proves a 40 KiB cap does not drop an image.
3. `readfile`: non-UTF-8 rule. Tests: a Latin-1 CSV returns U+FFFD, a NUL in the first 8 KiB is refused, and a bad byte before the offset no longer refuses.
4. `readfile/image.go`: decode, downscale and re-encode. Tests use generated PNG/JPEG/GIF fixtures, oversize, the megapixel bomb, and webp refused.
5. `readfile/office.go`: docx, xlsx and pptx. Fixtures are built in the test with `archive/zip`, plus a zip bomb test.
6. `readfile/pdf.go`: pdftotext. The test builds a tiny PDF fixture and skips when the binary is absent, like rg. Also tests for no text layer and a missing binary.
7. Adapters: anthropic, chatgpt, openai and claudesdk render `Images`. Each gets a request-shape test.
8. Live check of the two uncertain points, then `make gate` and the PR.
