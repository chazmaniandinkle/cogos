# Writing a Provider

CogOS routes inference through the `Provider` interface. The kernel ships with providers for Anthropic, Ollama, Claude Code, and Codex, but you can add your own.

## The Interface

A provider implements six methods:

```go
type Provider interface {
    // Name returns the provider identifier (e.g. "ollama", "anthropic").
    Name() string

    // Available reports whether the provider is ready to serve requests.
    Available(ctx context.Context) bool

    // Capabilities returns what this provider supports.
    Capabilities() ProviderCapabilities

    // Ping probes the endpoint and returns measured latency.
    Ping(ctx context.Context) (time.Duration, error)

    // Complete sends a request and waits for the full response.
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

    // Stream sends a request and returns a channel of incremental chunks.
    Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
}
```

## Capabilities

Declare what your provider supports so the router can make informed decisions:

```go
func (p *MyProvider) Capabilities() ProviderCapabilities {
    return ProviderCapabilities{
        Capabilities:    []Capability{CapStreaming, CapToolUse},
        MaxContextTokens:   128_000,
        MaxOutputTokens:    4_096,
        ModelsAvailable:    []string{"my-model-7b"},
        IsLocal:            true,  // affects sovereignty gradient scoring
        CostPerInputToken:  0,     // 0 for local models
        CostPerOutputToken: 0,
    }
}
```

Key fields:
- **`IsLocal`** — Local providers are scored higher by default (sovereignty gradient). Set `true` for on-device inference.
- **`CostPerInputToken` / `CostPerOutputToken`** — Used by the router for cost-aware routing. Set to `0` for local or subscription-based providers.
- **`Capabilities`** — The router filters providers by required capabilities. Available: `CapStreaming`, `CapToolUse`, `CapVision`, `CapLongContext`, `CapCaching`.
- **`AgenticHarness`** — Set `true` if the provider owns its own tool loop (like Claude Code).

## Streaming

The `Stream` method returns a `<-chan StreamChunk`. Send text deltas as they arrive and close the channel when done:

```go
func (p *MyProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
    ch := make(chan StreamChunk, 32)

    go func() {
        defer close(ch)

        // ... connect to your model, read response stream ...

        for token := range modelStream {
            select {
            case ch <- StreamChunk{Delta: token}:
            case <-ctx.Done():
                ch <- StreamChunk{Error: ctx.Err(), Done: true}
                return
            }
        }

        ch <- StreamChunk{
            Done: true,
            Usage: &TokenUsage{
                InputTokens:  inputCount,
                OutputTokens: outputCount,
            },
            ProviderMeta: &ProviderMeta{
                Provider: p.Name(),
                Model:    "my-model-7b",
                Latency:  elapsed,
            },
        }
    }()

    return ch, nil
}
```

If your provider doesn't support streaming natively, fall back to `Complete` and emit a single chunk:

```go
func (p *MyProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
    resp, err := p.Complete(ctx, req)
    if err != nil {
        return nil, err
    }
    ch := make(chan StreamChunk, 1)
    ch <- StreamChunk{
        Delta:        resp.Content,
        Done:         true,
        Usage:        &resp.Usage,
        ProviderMeta: &resp.ProviderMeta,
    }
    close(ch)
    return ch, nil
}
```

## Configuration

Providers are configured in `providers.yaml`:

```yaml
providers:
  my-provider:
    type: my-provider        # matches your registration key
    enabled: true
    endpoint: "http://localhost:8080"
    model: "my-model-7b"
    timeout: 120
    options:
      custom_option: "value"
```

The router reads this file at startup and instantiates enabled providers via `makeProvider()`.

## Registration

To register your provider with the kernel, add a case to `makeProvider()` in `router.go`:

```go
case "my-provider":
    return NewMyProvider(name, cfg), nil
```

## What the Router Does

You don't need to worry about routing logic — the router handles:

1. **Process-state overrides** — Different providers for different cognitive states
2. **Capability filtering** — Only routes to providers that support what the request needs
3. **Sovereignty gradient** — Scores local providers higher than cloud
4. **Availability checking** — Skips providers that aren't responding
5. **Fallback chains** — Tries the next provider if the preferred one fails
6. **Decision recording** — Every routing decision is logged for future sentinel training

## Existing Providers as Reference

| Provider | File | Notes |
|----------|------|-------|
| Anthropic | `provider_anthropic.go` | Direct API calls, streaming SSE parsing, cache-aware |
| Ollama | `provider_ollama.go` | Local inference, OpenAI-compatible endpoint |
| Claude Code | `provider_claudecode.go` | Agentic — spawns `claude -p` subprocesses |
| Codex | `provider_codex.go` | OpenAI Codex CLI integration |
| Stub | `provider_stub.go` | Test double, useful as a minimal template |

Start with `provider_stub.go` as a skeleton if you're writing a new provider from scratch.
