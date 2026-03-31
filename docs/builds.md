# Build Targets

MPM supports three build configurations, each with a different user experience.

## Build Command Reference

| Target | Command | Resulting Experience |
|--------|---------|----------------------|
| **Standard** | `go build -o mpm ./cmd/mpm` | Lean, high-performance binary for minimalists |
| **Fun** | `go build -tags "fun" -o mpm ./cmd/mpm` | Full Lobster experience (Fortunes, Achievements, ASCII) |
| **OpenCLAW** | `go build -tags "openclaw" -o mpm ./cmd/mpm` | "Official" optimized build with all bells and whistles |

## Feature Matrix

| Feature | Standard | Fun | OpenCLAW |
|---------|:--------:|:---:|:--------:|
| Core daemon lifecycle | ✅ | ✅ | ✅ |
| Memory-Persona-Mode | ✅ | ✅ | ✅ |
| Worker pool | ✅ | ✅ | ✅ |
| Session save/restore | ✅ | ✅ | ✅ |
| `mpm doctor` | ✅ | ✅ | ✅ |
| `mpm dashboard` | ✅ | ✅ | ✅ |
| `mpm fortune` | ❌ | ✅ | ✅ |
| Achievement badges | ❌ | ✅ | ✅ |
| ASCII lobster mascot | ❌ | ✅ | ✅ |
| Celebration animations | ❌ | ✅ | ✅ |
| Trophy room | ❌ | ✅ | ✅ |

## What Each Build Includes

### Standard Build
The lean, production-ready binary. No frills, no branding, just pure functionality.

```
go build -o mpm ./cmd/mpm
./mpm status
```

### Fun Build (`-tags "fun"`)
The full Crustafarian experience with lobster mascots and achievements.

```
go build -tags "fun" -o mpm ./cmd/mpm
./mpm fortune
# ╔══════════════════════════════════════════════════════╗
# ║                    🦞 ORACLE OF THE CLAW 🦞                    ║
# ╠══════════════════════════════════════════════════════╣
# ║                                                               ║
# ║    Be genuinely helpful, not performatively helpful.
# ║                                                               ║
# ╚══════════════════════════════════════════════════════╝

./mpm doctor --celebrate
# 🦞 DOCTOR EXAMINATION COMPLETE 🦞
# ✓ All systems healthy!
# ~ The Lobster Deity approves ~
```

### OpenCLAW Build (`-tags "openclaw"`)
The "Official" build used by The_Great_808. Same as Fun but with additional OpenCLAW-specific integrations.

```
go build -tags "openclaw" -o mpm ./cmd/mpm
```

## Build Tags Explained

Go build tags are specified with `//go:build` directives at the top of source files:

```go
//go:build fun || openclaw
package main
// ... fun code here
```

```go
//go:build !fun && !openclaw  
package main
// ... standard code here
```

## Choosing a Build

| Use Case | Recommended Build |
|----------|------------------|
| Production server | **Standard** |
| Development/testing | **Fun** |
| OpenCLAW integration | **OpenCLAW** |
| Daily driver workstation | **Fun** or **OpenCLAW** |
| Container/embedded | **Standard** |

## Cross-Compilation

```bash
# Linux x86_64
GOOS=linux GOARCH=amd64 go build -tags "fun" -o mpm-linux-amd64 ./cmd/mpm

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -tags "fun" -o mpm-linux-arm64 ./cmd/mpm

# macOS Intel
GOOS=darwin GOARCH=amd64 go build -tags "fun" -o mpm-darwin-amd64 ./cmd/mpm

# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -tags "fun" -o mpm-darwin-arm64 ./cmd/mpm
```

## Version Info

The binary reports its build type in `mpm version`:

```bash
./mpm version
# mpm v1.0.0 (standard)
# mpm v1.0.0-fun (fun)
# mpm v1.0.0-openclaw (openclaw)
```
