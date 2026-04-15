# Synth Command — LLM Session Summarization

The `mpm synthesize <uuid>` command uses an LLM to analyze a session transcript and extract structured facts, topics, and a summary.

## Configuration

All settings are configured via the `synth` section in `mpm_config.json`. Environment variables serve as fallbacks.

**Example `mpm_config.json`:**
```json
{
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "your-api-key-here",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model` | string | `"MiniMax-M2.7"` | Model name (e.g. `MiniMax-M2.7`, `llama3`, `gpt-4o`) |
| `api_key` | string | `MINIMAX_API_KEY` env var | API key for the LLM provider |
| `base_url` | string | Provider-specific defaults | Endpoint URL (see below) |
| `max_tokens` | int | `1024` | Max tokens in response |
| `timeout_seconds` | int | `300` | Request timeout in seconds |

### Base URL Defaults

If `base_url` is not set, the following defaults are used based on model:

| Model | Default Base URL |
|-------|-----------------|
| `MiniMax-M2.7` / `minimax/MiniMax-M2.7` | `https://api.minimax.io/anthropic` |
| `llama3`, `mixtral`, `codellama`, or other local models | `http://localhost:11434/v1` |

### Environment Variable Fallback

If `api_key` is not set in config, `MINIMAX_API_KEY` is used as fallback. If neither is set, synthesis fails with a clear error message.

## Supported Providers

Any OpenAI-compatible API works. Set `base_url` appropriately:

| Provider | Example `base_url` |
|----------|-------------------|
| MiniMax | `https://api.minimax.io/anthropic` |
| OpenAI | `https://api.openai.com/v1` |
| Ollama (local) | `http://localhost:11434/v1` |
| LM Studio | `http://localhost:1234/v1` |

## Usage

```bash
mpm synthesize <session-uuid>   # Synthesize a specific session
mpm session list               # Find session UUIDs
```

## How It Works

1. Reads the session's JSONL transcript (or falls back to sessions table)
2. Builds a synthesis prompt with the transcript
3. Calls the LLM with a structured output request (JSON response expected)
4. Parses the response: extracts facts (memories), topics, summary
5. Handles `null` array responses gracefully
6. Stores each fact as a memory with `synthesized: true` tag
7. Stores the session summary in the sessions table

## Output Format

The LLM is asked to return structured JSON:

```json
{
  "session_summary": "User discussed project architecture...",
  "topics": ["project", "architecture", "go"],
  "memories": [
    "User is building a multi-agent system",
    "Go is the primary language",
    "SQLite is used for memory storage"
  ]
}
```

If the LLM returns `null` for `topics` or `memories`, this is handled gracefully (treated as empty arrays).

If the session was transient or yielded no memorable facts, a message is shown instead of storing empty facts.

---
**Last Updated:** 2026-04-09