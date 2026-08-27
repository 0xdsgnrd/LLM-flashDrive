# LLM-flashDrive

A portable LLM on a USB drive. Plug it into any Mac, Windows, or Linux machine —
double-click one file — chat with a local model. **No install, no internet, no admin rights.**

## The one rule

**Docker builds it. Docker never ships on it.**

```
YOUR MACHINE (build time)              THEIR MACHINE (run time)
──────────────────────────             ────────────────────────
Docker  → compiles linux/win binaries  nothing installed
Node    → dev server + UI                       │
        └──────► /Volumes/ai-Drive ─────────────┘
                 native binaries · .gguf · static HTML
```

A compiled binary carries no trace of its build environment. The end user needs nothing.

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

`llama-server --path ui/` serves the UI **and** the `/v1/*` API from one origin.
Nothing to configure, nothing to whitelist. `scripts/devserver.mjs` mirrors this in
development by proxying, so dev and production behave identically.

## Layout

```
repo (internal SSD — never develop on exFAT)
├── ui/                     zero-dependency chat UI (no CDN, works offline)
├── runtime/                launchers copied to the drive
├── docker/                 dev image + linux/windows cross-build images
├── scripts/
│   ├── build-mac.sh        native Metal build
│   ├── build-linux.sh      static, old glibc, GGML_NATIVE=OFF
│   ├── build-windows.sh    mingw-w64 cross-compile, fully static
│   ├── fetch-model.sh      pull GGUFs straight to the drive
│   ├── devserver.mjs       static server + API proxy
│   └── release.sh          stage everything → /Volumes/ai-Drive
└── dist/                   build outputs (gitignored)
```

## Workflow

```bash
# 1. build for each platform you want to support
brew install cmake && ./scripts/build-mac.sh
./scripts/build-linux.sh
./scripts/build-windows.sh

# 2. get a model (lists available files if you omit the filename)
./scripts/fetch-model.sh <hf-repo>
./scripts/fetch-model.sh <hf-repo> <file.gguf>

# 3. stage to the drive
./scripts/release.sh

# 4. develop the UI against the host's native server
ollama serve                    # or a native llama-server on :11434
docker compose up dev           # → http://localhost:5173
```

## The model ladder

Launchers detect RAM and load the **largest model that fits in 60% of it**
(the rest goes to KV cache and the OS). Ship several sizes so the drive works
on any machine:

| Host RAM | Model that loads |
|---|---|
| 8 GB | ~4B |
| 16 GB | ~8B |
| 32 GB | ~32B |
| 64 GB+ | ~70B |

A drive holding only a 70B is useless on the 16GB laptops most people own.

## Portability gotchas, already handled

- **`GGML_NATIVE=OFF`** on cross-builds — otherwise the binary bakes in the build
  machine's AVX-512 and dies with SIGILL on older CPUs.
- **Old glibc base (bullseye)** — build on new glibc and it fails with `GLIBC_2.xx not found`.
- **Fully static Windows build** — no mingw runtime DLLs required on the target.
- **Ad-hoc codesign on arm64** — required by macOS; survives the copy to exFAT.
- **CRLF for `.bat`/`.ps1`, LF for `.sh`/`.command`** — enforced in `runtime/`.
- **Quarantine stripped** at release, so Gatekeeper doesn't block on other Macs.
- **`-ExecutionPolicy Bypass`** so the PowerShell launcher runs without admin.
- **exFAT has no journal** — always eject cleanly; never put a git repo or
  `node_modules` on the drive.

## Known friction (cosmetic)

- Windows SmartScreen warns on unsigned executables from removable media → "More info → Run anyway".
- Some Linux distros automount USB `noexec`; the launcher detects this and prints the remount command.
