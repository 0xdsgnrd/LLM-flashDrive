# flashDrive-LLM

A portable LLM on a USB drive. Plug it into any Mac, Windows, or Linux machine —
double-click one file — chat with a local model. **No install, no internet, no admin rights.**

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
                                                 ├─ /api/*         (conversations)
                                                 └─ /v1/*  ──► llama-server :8081

without it              browser → llama-server :8080 ── ui/ + /v1/*   (history off)
```

`scripts/devserver.mjs` has always done the proxying half of this in development;
pocketd is that file, compiled and portable.

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
./erase-chats.command      # on the drive: erase every conversation
```

The app's sidebar has the same button, and `verify-drive.sh` reports how many
conversations are on the drive so you know before handing it to someone. This is
a plain delete: exFAT does not overwrite the bytes, so it defends against the next
person to plug the drive in, not against someone with recovery tools.

## Layout

```
repo (internal SSD — never develop on exFAT)
├── ui/                     zero-dependency chat UI (no CDN, works offline)
├── helper/                 pocketd — front door, proxy, and chat storage (Go, stdlib only)
├── runtime/                launchers + erase-chats.command, copied to the drive
├── docker/                 dev image + linux/windows cross-build images
├── scripts/
│   ├── build-mac.sh        native Metal build
│   ├── build-linux.sh      static, old glibc, GGML_NATIVE=OFF
│   ├── build-windows.sh    mingw-w64 cross-compile, fully static
│   ├── build-helper.sh     pocketd for all three platforms (no Go install needed)
│   ├── fetch-model.sh      pull GGUFs straight to the drive
│   ├── devserver.mjs       static server + API proxy
│   ├── verify-drive.sh     check the drive against drive.lock
│   └── release.sh          stage everything → /Volumes/Pocket-LLM
└── dist/                   build outputs (gitignored)
```

## Workflow

```bash
# 1. build for each platform you want to support
brew install cmake && ./scripts/build-mac.sh
./scripts/build-linux.sh
./scripts/build-windows.sh
./scripts/build-helper.sh       # pocketd; needs Docker, not Go


# 2. get a model (lists available files if you omit the filename)
./scripts/fetch-model.sh <hf-repo>
./scripts/fetch-model.sh <hf-repo> <file.gguf>

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
- **`pocketd` sidesteps all of the C++ traps above.** `CGO_ENABLED=0` produces a
  binary with no libc dependency at all, so there is no glibc version to match, no
  `libgomp`, and no mingw threading model to get wrong. It is the cheap half of
  the build.
- **CRLF for `.bat`/`.ps1`, LF for `.sh`/`.command`** — enforced in `runtime/`.
- **Quarantine stripped** at release, so Gatekeeper doesn't block on other Macs.
- **`-ExecutionPolicy Bypass`** so the PowerShell launcher runs without admin.
- **exFAT has no journal** — always eject cleanly; never put a git repo or
  `node_modules` on the drive.

## Reproducible drive contents

`drive.lock` pins every model to a repo **revision SHA** and **sha256**, and
pins the llama.cpp tag the binaries were built from.

```bash
./scripts/lock-add.sh <hf-repo> <file.gguf>   # resolve + pin a new model
./scripts/verify-drive.sh                     # presence + byte size (seconds)
./scripts/verify-drive.sh --sha               # + sha256 (minutes, reads everything)
```

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
