# Pocket-LLM

A portable LLM on a USB drive. Plug it into any Mac, Windows, or Linux machine —
double-click one file — chat with a local model. **No install, no internet, no admin rights.**

## Start here

Three ways to arrive at this repo, and only one of them needs the rest of it.

| | |
|---|---|
| **Handed a finished drive** | Plug it in and double-click `run-mac.command`, `run-linux.sh` or `run-windows.bat`. Nothing installs, nothing phones home, nothing is left behind on the host. |
| **Building a drive** | [Make your own drive](#make-your-own-drive) — one command, no toolchain. |
| **Changing the code** | [Workflow (building from source)](#workflow-building-from-source). |

Everything between here and there is why it is built this way, not how to use it.

## Make your own drive

Requires a Mac, a stick of 64GB or more, and patience with the connection.
Docker, cmake, Go and Xcode are not needed — `git`, `curl` and `python3` ship
with macOS and are the entire dependency list.

Building a drive is macOS-only, because finding and ejecting a removable volume
goes through `diskutil`. Running a finished drive works on all three platforms,
which is the part that matters.

**1. Format the stick as exFAT.** It is the only filesystem all three platforms
mount read-write without extra software, and a new stick is almost never
formatted that way already.

Disk Utility → select the stick → Erase → Format **ExFAT**, Scheme **Master Boot
Record**. Naming it `Pocket-LLM` saves typing later: `provision.sh` finds the
stick whatever it is called, but `release.sh`, `fetch-model.sh` and
`verify-drive.sh` default to `/Volumes/Pocket-LLM` and need `DRIVE=/Volumes/NAME`
to look anywhere else.

The command-line equivalent, checking the size reported by the first line before
running the second — `eraseDisk` does not ask twice:

```bash
diskutil list external physical
diskutil eraseDisk ExFAT POCKET MBRFormat /dev/diskN
```

**2. Clone and run.**

```bash
git clone https://github.com/0xdsgnrd/Pocket-LLM
cd Pocket-LLM
./scripts/provision.sh
```

That is the whole install. The stick is found, its format checked, its capacity
read, the largest set of models that fits selected, downloaded, verified and
ejected. `--plan` prints the intent and changes nothing.

**3. Wait.** The models are the download — 38GB for a 64GB stick, 112GB for a
256GB one. An hour at best, an afternoon at worst. It streams to the stick and
never touches the internal disk.

Stopping it is safe, and so is unplugging mid-download. Re-running resumes,
because what is still missing is worked out from the drive rather than
remembered.

**4. Plug it into anything** and double-click the launcher for that platform.

### Before a drive changes hands

```bash
/Volumes/NAME/verify-drive.sh --sha
```

Provisioning proves file sizes as it goes; this proves the bytes. Worth one run,
because exFAT has no journal and an interrupted write can leave a file at exactly
the right length with the wrong contents inside it.

### Licences on the weights

Qwen, gpt-oss and Mistral Small are Apache-2.0. Gemma carries Google's own terms
and DeepSeek its own. Handing out drives is redistribution, and the licences are
worth ten minutes before that becomes a habit.

## Docker is the compiler, not the runtime

Linux and Windows binaries can't be built on a Mac, and the Linux one has to link
against a deliberately *old* glibc (bullseye, 2.31) so it still runs on distros from
2020 onward. Containers solve both — pinned toolchains, repeatable results, nothing
extra installed on your machine.

None of it reaches the drive.

```
YOUR MACHINE (build time)              THEIR MACHINE (run time)
──────────────────────────             ────────────────────────
Docker  → compiles linux/win binaries  nothing installed
Node    → dev server + UI                       │
        └──────► /Volumes/Pocket-LLM ───────────┘
                 native binaries · .gguf · static HTML
```

A compiled binary carries no trace of its build environment, so the machine you plug
into needs nothing at all.

## The GPU wall

Docker Desktop on macOS runs containers in a Linux VM with **no access to Metal**.
Containerised inference on Apple Silicon falls back to CPU inside a VM — a 70B drops
from ~30 tok/s to low single digits, and Docker's default ~8GB VM can't even load it.

So the split is absolute:

| Layer | Where it runs | Why |
|---|---|---|
| Inference | **Host, native** | needs Metal |
| UI / RAG / tooling | Container | no GPU needed, reproducible |
| Cross-compilation | Container | can't build Linux/Windows on macOS |

The dev container proxies to the host's native server via `host.docker.internal`.

## No CORS, by design

The browser talks to exactly one origin, so there is nothing to configure and
nothing to whitelist. Which process is behind that origin depends on whether the
helper is present:

```
with pocketd            browser → pocketd :8080 ─┬─ ui/            (static)
                                                 ├─ /api/chats     (conversations)
                                                 ├─ /api/docs      (documents)
                                                 ├─ /api/search    (BM25 + vectors)
                                                 ├─ /v1/*  ──► llama-server :8081  (chat, router)
                                                 └─ ─────────► llama-server :8082  (encoder)

without it              browser → llama-server :8080 ── ui/ + /v1/*   (history off)
```

The encoder on :8082 is never reachable from the browser. It exists only to
answer pocketd, which is why adding it changed nothing about the single-origin
property that makes CORS a non-topic here.

`scripts/devserver.mjs` has always done the proxying half of this in development;
pocketd is that file, compiled and portable.

## The interface

Still one HTML file, one stylesheet and one script — no framework, no build step,
no webfonts, no CDN. It has to render from a stick on a machine with nothing
installed and no network, so every icon is an inline SVG sprite and every colour
is a CSS variable.

What it does, in the order it matters:

- **Markdown is actually parsed** — headings, nested lists, tables, quotes, rules,
  links, emphasis. A local model answers in markdown whether or not the UI
  understands it, so "minimal markdown" was not a smaller feature; it was tables
  rendered as rows of pipes.
- **Code blocks** carry the language and a copy button, with enough highlighting
  to give code shape. Deliberately not a grammar for any language: one pass over
  comments, strings, numbers and common keywords.
- **Copy and regenerate** on a message. Regenerate truncates the answer from the
  transcript rather than appending a second one, so a reload does not show both
  attempts.
- **Conversations grouped by age** — Today, Yesterday, Previous 7 days — with a
  search box, because that is how people remember a conversation.
- **The model picker** bands models Fast / Balanced / Best quality, shows what
  each one costs, and greys out what this machine cannot run. Sizes and fit come
  from `machine.json`, which only the launcher writes — so the bands fall back to
  the parameter count in the filename, and the picker stays grouped even when
  pocketd is run on its own.
- **Which retriever is running** is stated under the documents switch —
  *Matching words and meaning*, or *Matching words only* on a drive with no
  encoder. "Why did it not find that?" has a different answer depending on
  which half is live, and the difference is otherwise invisible.
- **Light and dark** both first-class. The system setting decides until the
  switch in the top bar is used; that choice is then stored on the drive with
  everything else, so a borrowed laptop still keeps nothing.

Everything renders escaped: HTML in an answer, in a table cell, in a code fence
or in a document title is shown as text, never as markup.

## Conversations stay on the drive

Chat history used to live in the browser's `localStorage`, which meant it stayed
behind on every machine the drive was plugged into and never travelled with the
stick. Both halves of that are wrong for a device whose whole promise is "no
install, no trace."

So `pocketd` owns storage. One conversation is one append-only `.jsonl` file in
`chats/` on the drive: a header line, then one line per message.

**Append-only is not a style choice.** exFAT has no journal and this drive gets
unplugged by people, not by software. Rewriting a whole transcript each turn puts
the entire conversation at risk on every message; appending one line risks only
that line, and a torn final line — no trailing newline — is detected and dropped
on read.

The UI writes nothing to the browser at all, not even the model preference, which
lives in `chats/settings.json` instead. **If pocketd cannot start** — a locked
laptop, a read-only drive — llama-server serves the UI alone, there is no `/api`,
and the sidebar says history is off. It does *not* quietly fall back to browser
storage, because that is precisely the behaviour being removed.

```bash
./erase-chats.command       # on the drive: erase every conversation
./erase-documents.command   # ...and every indexed document, separately
```

The app's sidebar has the same buttons, and `verify-drive.sh` reports how many
conversations and documents are on the drive so you know before handing it to
someone. This is a plain delete: exFAT does not overwrite the bytes, so it defends
against the next person to plug the drive in, not against someone with recovery
tools. The two are separate on purpose — a reference set you are happy to pass on
is a different thing from your conversations.

## Retrieval: words, and now meaning

Drop text files onto the window, tick **Use in answers**, and each question is
searched against them first; the best passages are prepended to that one request
and the answer cites which documents it used.

Search used to be BM25 alone, and BM25 alone has a hole in it big enough to see
from across the room: ask about **cars** and a document that only ever says
*automobile* never comes back, because the two share no term. So the drive now
runs a second retriever over the same passages — a small encoder that turns text
into a vector, where "car" and "automobile" land next to each other.

**Both, not one.** Replacing BM25 with vectors would have swapped one hole for
another. An encoder is vague about exactly the things BM25 is exact about: a part
number, an error code, a surname, a flag like `GGML_OPENMP`. Those are rare
tokens with enormous IDF and almost no semantic neighbourhood, and a dense
retriever will cheerfully return a passage that is *about* the same subject
instead of the one containing the string you typed. Measured on this drive:

| question | found by |
|---|---|
| `cars` → the automobile document | semantic only — no shared word |
| `GGML_OPENMP` → the build-flags note | both |
| `internal combustion engine` | both |
| `xylophone marsupial quantum` | neither, correctly |

### Fusing two rankings that do not share a scale

BM25 scores are unbounded and corpus-relative. Cosine lives in [-1, 1] and, for
most encoders, in a narrow band near the top of it. Adding them is meaningless,
and normalising each list to [0, 1] is worse than it looks: it stretches whatever
noise sits at the bottom of one list up to meet the signal at the top of the
other.

So the lists are fused by **rank**, not score — Reciprocal Rank Fusion, `1/(60+r)`
summed across retrievers. Position is the one thing both retrievers agree on the
meaning of, and a passage found by both rises above one found by either.

### The floor, and why the obvious version does not work

BM25 has a property worth keeping: no shared term, no result. Dense retrieval has
no such thing — every passage has a cosine against every query — so without a
floor, an unrelated question grounds its answer in whatever happened to be
nearest. It cannot be a fixed cosine, either: 0.5 is "unrelated" for one encoder
and "closely related" for another.

The first attempt was to keep passages some number of standard deviations above
the mean similarity **for that query**. Measured on bge-small over a ten-document
corpus, it fails outright:

```
"how do I make bread rise"    → baking.md    z = +2.20   (right answer)
"xylophone marsupial quantum" → music.md     z = +2.48   (pure noise)
```

The noise scores *higher*. When a query means nothing to the encoder the
similarities collapse into a narrow band, the standard deviation shrinks with
them, and whichever passage happens to lead is left standing several deviations
clear of a mean it is barely above. Dividing by a quantity that collapses exactly
when the answer should be "nothing here" cannot work.

So the yardstick is the corpus instead of the query: **what two unrelated
passages on this drive actually look like to this encoder**, measured from a
sample of passage pairs. Same corpus, same encoder, same queries, scored against
that baseline:

```
relevant    5.12   4.08   2.88   4.12
noise       0.82   0.76  -0.14
```

which separates with room to spare. Nothing about it is tuned to a model: the
baseline is remeasured whenever the vectors change, so it calibrates itself to
whatever encoder the drive happens to carry. Pairs drawn from the same document
are excluded — consecutive chunks deliberately overlap, and two halves of one
paragraph are not evidence of what "unrelated" means.

### A separate server, not another model in the router

The encoder lives in `embed/`, not `models/`, and gets its own `llama-server`
process. That is not tidiness. The router is started with `--models-max` derived
from host RAM, and on an 8GB machine that number is **1** — so an encoder sharing
the router would be evicted by every search and the chat model reloaded for every
answer. A dedicated process holds ~320MB and never contends. Its size is
subtracted from the RAM budget before the model-packing arithmetic, so it is
spent once rather than counted twice.

`embed/` empty is a supported configuration, not a broken one. No encoder, a
server that never comes up, a query embedding that misses its 3-second deadline —
each leaves BM25 answering alone, exactly as this drive did before, and the
sidebar says *Matching words only* rather than pretending.

### Embeddings are cached on the drive; the lexical index still is not

There is still **no lexical index file**, for the reason there never was: it
rebuilds from `docs/` in milliseconds, so a format that could be stale or
half-written buys nothing.

Vectors are the opposite — encoding a corpus is minutes of real compute, and
doing it on every launch would make the drive unusable on the machine you just
plugged it into. So `docs/<id>.vec` exists, and is written to be thrown away:

- **Content-addressed.** Each vector is keyed by a hash of the passage text, not
  its position. Re-adding a file, re-chunking, reordering documents — none of it
  can pair a vector with the wrong passage. Adding one note to a drive holding a
  thousand embedded passages encodes one passage.
- **Self-invalidating.** The header records the encoder and dimension. Swap the
  model in `embed/` and every cache is discarded on read rather than blending two
  incompatible vector spaces into one ranking.
- **Torn writes are free.** The payload must be exactly `count × record size`; if
  it is not, the file is dropped and the vectors recomputed. Transcripts are
  append-only because losing one costs you a conversation. Losing this costs a
  few minutes of background work.

Deleting every `.vec` on the drive has exactly one consequence: the next launch
is busy for a while. `erase-documents.command` deletes them along with the
documents — a vector is a lossy but real representation of the text it came from,
so leaving it behind would leave part of the document behind.

### Everything else about retrieval, unchanged

- **Encoding never blocks anything.** Dropping in a 300-page PDF returns as soon
  as the text is extracted and the lexical index is rebuilt — searchable by word
  immediately — and the vectors arrive behind it while the drive is in use. The
  sidebar counts down: *learning meanings 200/900*.
- **Chunks are paragraph-aligned**, ~1000 characters with 150 of overlap.
- **At most two passages per document**, so one long file cannot crowd out every
  other source. This applies to the fused ranking, so it holds through the dense
  path too.
- **The retrieved block is never stored.** It is prepended to a single request,
  never enters the transcript, is not replayed on the next turn and does not eat
  context forever. Only the document *names* are saved, as the citation line.
- **Retrieval failing never blocks an answer.** If search errors, the question
  goes to the model ungrounded.
- **Formats are extracted, not guessed at.** See the table below.

### What can be added

| Format | Handled by |
|---|---|
| `.txt` `.md` `.csv` `.json` `.log` `.css`, source files, anything that reads as text | read directly |
| `.pdf` | `github.com/ledongthuc/pdf`, vendored |
| `.docx` `.pptx` `.xlsx` | `archive/zip` + `encoding/xml` |
| `.rtf` `.html` `.xml` | text stripping |
| `.zip` `.tar` `.tar.gz` `.tgz` | one document per member, named `archive.zip → path/inside.md` |

`.pdf` is the only dependency in the project; everything else here is stdlib. It
is vendored under `helper/vendor/` (BSD-3, Go Authors — licence retained there)
so the build needs no network and cannot drift.
Uploads are raw bytes with the name in the query string — reading a file to a
string in the browser would corrupt every binary format, and base64 would inflate
each upload by a third for nothing.

**Refused on purpose, by name:** `.doc`, `.ppt`, `.xls` are OLE compound binaries
with no usable Go extractor — the app says "re-save it as .docx" rather than
failing vaguely. `.rar` and `.7z` would each mean shipping a whole decompressor
for a format people rarely hand over; extract them first. A **scanned PDF** with
no text layer is detected and reported as needing OCR, which is not obvious to
someone looking at a page full of visible words.

Some deliberate limits: archives nested inside archives are not unpacked (that is
where a decompression bomb hides), at most 300 members and 64MB of extracted text
per upload, `__MACOSX/` and dotfiles are skipped, and `<script>`/`<style>` never
reach the index — minified JavaScript would otherwise flood it with junk tokens.

## Layout

```
repo (internal SSD — never develop on exFAT)
├── ui/                     zero-dependency chat UI (no CDN, works offline)
├── helper/                 pocketd — front door, proxy, storage, extraction, hybrid search (Go)
├── runtime/                launchers + erase scripts, copied to the drive
├── docker/                 dev image + linux/windows cross-build images
├── scripts/
│   ├── build-mac.sh        native Metal build
│   ├── build-linux.sh      static, old glibc, GGML_NATIVE=OFF
│   ├── build-windows.sh    mingw-w64 cross-compile, fully static
│   ├── build-helper.sh     pocketd for all three platforms (no Go install needed)
│   ├── fetch-model.sh      pull GGUFs straight to the drive (--embed for the encoder)
│   ├── devserver.mjs       static server + API proxy
│   ├── verify-drive.sh     check the drive against drive.lock
│   ├── release-binaries.sh build all six, publish a release, pin their sha256
│   ├── provision.sh        blank stick → finished drive, no toolchain needed
│   └── release.sh          stage everything → /Volumes/Pocket-LLM
└── dist/                   build outputs (gitignored)
```

## How provisioning works

The workflow splits at a seam, and the two halves have completely different
requirements:

```bash
# MAINTAINERS ONLY — once per code change.
# Needs a Mac (Metal, codesigning), Docker, and push access to the repo.
./scripts/release-binaries.sh v1.0.0

# EVERYONE — once per stick. Needs curl and bandwidth, nothing else.
./scripts/provision.sh
```

**If you are making a drive for yourself, you only ever run the second one.**
The binaries are already published; `release-binaries.sh` would try to create a
release in a repo you cannot write to. It exists for whoever maintains a fork —
and if you do publish your own, it re-pins `drive.lock` to your repo, so your
users fetch your binaries rather than someone else's.

That split is the entire design. Before it, a drive cost cmake, Docker and a
Mac. Now it costs bandwidth.

`provision.sh` finds the external drive itself, refuses to guess when two are
mounted, reads the capacity, picks the largest profile that fits, fetches the
binaries from the pinned release and the models from Hugging Face, verifies
everything and ejects.

**It is a convergence loop, not a sequence of steps.** Every run asks the drive
what it is still missing and fixes that; nothing assumes a clean start. So there
is no resume logic to get wrong — interrupt it, unplug the stick, re-run it, and
it continues, because "where it stopped" is derived from the drive rather than
remembered. A 111GB profile over a throttled connection is several hours, and a
process you cannot safely kill is a process you cannot walk away from.

### Profiles

Named after the stick you are holding, because that is the decision you are
actually making. Each model is tagged in `drive.lock` with the smallest drive it
belongs on, so "the 64 set" is a filter rather than set arithmetic.

| profile | adds | total | covers |
|---|---|---|---|
| **64** | encoder, Qwen3-4B, Qwen3-8B, Gemma-12B, gpt-oss-20b, Mistral-24B | 37.7 GiB | 8 / 16 / 32GB laptops |
| **128** | + Gemma-26B-A4B, Qwen3-32B | 71.9 GiB | + more choice at 32GB |
| **256** | + DeepSeek-R1-70B | 111.5 GiB | + 64GB workstations |

The 64 profile drops only models that need 32GB+ of host RAM anyway, so a cheap
stick loses far less than its size suggests. Room is reserved on every profile
for conversations, documents and the vector cache — a drive filled to the brim
is a drive that fails the first time somebody uses it.

### What it will not do

- **Format anything.** The stick must already be exFAT, the one filesystem
  macOS, Windows and Linux all mount read-write. FAT32 is rejected *by name*,
  because its 4GB per-file limit fails on every model here and the error you
  would otherwise get is about a failed write, not about the filesystem.
- **Choose between two drives.** One external volume is used; two is an error
  that lists them. Nothing should guess which disk gets 100GB written to it.
- **Delete anything.** Models on the drive that are outside the chosen profile
  are reported with their sizes so you can remove them yourself. Freeing 40GB is
  not a call a script should make on its own.

### Binaries are pinned like models

`release-binaries.sh` builds all six, publishes them to a GitHub release, and
writes their sha256 into `drive.lock`. Provisioning verifies each one before
staging it, so a binary is never trusted on the strength of the URL it came
from.

That also fixes something this project has admitted for its whole life: the
verifier could only ever run `--version` against the host's own binary, and
took the Linux and Windows ones on faith. A hash needs no CPU that understands
the file, so `verify-drive.sh --sha` now checks all six.

Release assets share one flat namespace and three of these files are called
`llama-server`, so each asset is named for its path with the separator swapped —
`mac-arm64-llama-server`. Derivable in both directions, so the lock needs no
column to remember it.

## Workflow (building from source)

```bash
# 1. build for each platform you want to support
brew install cmake && ./scripts/build-mac.sh
./scripts/build-linux.sh
./scripts/build-windows.sh
./scripts/build-helper.sh       # pocketd; needs Docker, not Go


# 2. get a model (lists available files if you omit the filename)
./scripts/fetch-model.sh <hf-repo>
./scripts/fetch-model.sh <hf-repo> <file.gguf>

# 2b. get the retrieval encoder — optional; without it search is BM25 only
./scripts/fetch-model.sh --embed unsloth/embeddinggemma-300m-GGUF \
                                 embeddinggemma-300M-Q8_0.gguf

# 3. stage to the drive
./scripts/release.sh

# 4. develop the UI against the host's native server
ollama serve                    # or a native llama-server on :11434
docker compose up dev           # → http://localhost:5173
```

## Router mode and the model ladder

`llama-server --models-dir models/` serves **every** model on the drive and the
UI picks one per request, so a single drive covers different jobs (coding,
chat, fast/low-RAM) rather than one model per machine.

The RAM budget — **`min(70% of RAM, RAM - 4GB)`** — did not go away; it changed
role. It now decides two things:

1. **`--models-max`**, how many models may be resident at once. The llama-server
   default is 4, which is fatal on a small host: four 20GB models is 80GB. The
   launcher instead sizes N so that the N *largest* models still fit the budget.
2. **Which models the UI offers.** Router mode lists everything in `models/`,
   including files too large for the host, so the launcher writes
   `ui/machine.json` and the UI greys out what will not fit. Missing manifest =
   offer everything (fail open).

| Host RAM | Budget | `--models-max` | Offered (of 4B/8B/32B) |
|---|---|---|---|
| 8 GB | 4.0 GB | 1 | 4B |
| 16 GB | 11.2 GB | 2 | 4B, 8B |
| 32 GB | 22.4 GB | 2 | all three |
| 128 GB | 89.6 GB | 3 | all three |

Those budgets are before the encoder. One in `embed/` stays resident for the
whole session, so its size comes off the top before any of this arithmetic runs:
EmbeddingGemma-300M costs 0.31GB, which is why an 8GB machine's real budget is
3.7GB rather than 4.0GB. It is spent once, not counted against every model.

Both bounds in the budget are needed: the percentage alone starves big machines
(a 32GB box skips a 20GB model by 0.8GB), while the absolute floor alone lets an
8GB box load a 5GB model with 3GB left for the OS.

A drive holding only a 70B is useless on the 16GB laptops most people own.

## Portability gotchas, already handled

- **`GGML_NATIVE=OFF`** on cross-builds — otherwise the binary bakes in the build
  machine's AVX-512 and dies with SIGILL on older CPUs.
- **Old glibc base (bullseye 2.31)** — build on new glibc and it fails with `GLIBC_2.xx not found`.
- **NOT fully static on Linux.** glibc cannot be statically linked with NPTL/dlopen —
  `-static` fails at link time on `_dl_pagesize`, `_dl_stack_flags`, `_dl_init_static_tls`.
  Link glibc dynamically against an *old* glibc instead; it is forward compatible.
- **`GGML_OPENMP=OFF`** — otherwise the binary needs `libgomp.so.1`, which is absent from
  a minimal Linux install. llama.cpp's own threadpool replaces it.
- **mingw `-posix` compiler variants** for Windows — Debian's default mingw-w64 uses the
  win32 threading model, whose libstdc++ has no `std::thread`/`std::mutex`/
  `std::condition_variable`. Build fails with 131 "does not name a type" errors.
- **Fully static Windows build** — no mingw runtime DLLs required on the target.
- **Ad-hoc codesign on arm64** — required by macOS; survives the copy to exFAT.
  Applies to `pocketd` too, not just `llama-server`.
- **The encoder's identity must not be its path.** `llama-server` started with
  `-m` reports the model's *absolute path* as its id — `/Volumes/Pocket-LLM/embed/…`
  on macOS, `/media/<user>/…` on Linux, a drive letter on Windows. Keying the
  vector cache on that would invalidate it on arrival at every new machine and
  re-encode the whole corpus, which is the one cost the cache exists to avoid.
  The filename is the part that travels with the drive.
- **An oversized passage is refused, not truncated.** Retrieval encoders have
  small contexts — bge-small trains at 512 tokens — and `llama-server` answers an
  over-long input with `input (802 tokens) is larger than the max context size`,
  failing the whole batch with it. Inputs are sized against the context the
  server reports, and a refusal is halved and retried rather than being allowed
  to stall a document.
- **`pocketd` sidesteps all of the C++ traps above.** `CGO_ENABLED=0` produces a
  binary with no libc dependency at all, so there is no glibc version to match, no
  `libgomp`, and no mingw threading model to get wrong. It is the cheap half of
  the build.
- **Its one dependency is vendored** (`helper/vendor/`) and built with
  `-mod=vendor`, so the build needs no network and cannot drift.
- **CRLF for `.bat`/`.ps1`, LF for `.sh`/`.command`** — enforced in `runtime/`.
- **Quarantine stripped** at release, so Gatekeeper doesn't block on other Macs.
- **`-ExecutionPolicy Bypass`** so the PowerShell launcher runs without admin.
- **exFAT has no journal** — always eject cleanly; never put a git repo or
  `node_modules` on the drive.

## Reproducible drive contents

`drive.lock` pins every model to a repo **revision SHA** and **sha256**, and
pins the llama.cpp tag the binaries were built from.

```bash
./scripts/lock-add.sh <hf-repo> <file.gguf>            # pin a chat model
./scripts/lock-add.sh --embed <hf-repo> <file.gguf>    # pin the encoder
./scripts/verify-drive.sh                              # presence + byte size (seconds)
./scripts/verify-drive.sh --sha                        # + sha256 (minutes, reads everything)
```

`model` and `embed` entries carry identical fields; the keyword says which
directory the file belongs in on the drive, and therefore whether it is offered
in the picker or runs as the encoder. The verifier reports which of the two
retrievers the drive can actually do, because an absent encoder changes what
search can find without producing a single error.

**Size is not identity.** DeepSeek-R1-Distill-Llama-70B Q4_K_M and
Llama-3.3-70B-Instruct Q4_K_M differ by 2,368 bytes out of 42.5GB — a size check
cannot tell them apart. exFAT is also unjournaled, so an interrupted write can
leave a file at exactly the right length. Run `--sha` before trusting a drive
you did not fill yourself, or after any flaky transfer.

The verifier also reports models on the drive that are **not** pinned, lock
entries not yet downloaded, and whether the host's own binary was built from the
pinned commit (the cross-compiled ones cannot be executed to check).

## Verifying a build is actually portable

A build that succeeds locally proves nothing. After each one:

```bash
# macOS — must print nothing
otool -L dist/mac-arm64/llama-server | grep -v "/usr/lib/\|/System/"
otool -l dist/mac-arm64/llama-server | grep LC_RPATH     # no local paths

# Linux — every dep must resolve in a MINIMAL image, not your build image
docker run --rm --platform linux/amd64 -v "$PWD/dist/linux-x64:/x:ro" \
  debian:bullseye-slim ldd /x/llama-server        # no "not found"
```

`libgomp.so.1 => not found` was caught exactly this way, after a build that reported success.

## Known friction (cosmetic)

- Windows SmartScreen warns on unsigned executables from removable media → "More info → Run anyway".
- Some Linux distros automount USB `noexec`; the launcher detects this and prints the remount command.
