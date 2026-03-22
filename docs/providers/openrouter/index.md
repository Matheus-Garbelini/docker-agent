---
title: "OpenRouter"
description: "Use OpenRouter-routed chat and embedding models with docker-agent."
permalink: /providers/openrouter/
---

# OpenRouter

_Use OpenRouter-routed chat and embedding models with docker-agent._

## Setup

```bash
# Set your API key
export OPENROUTER_API_KEY="sk-or-..."
```

## Configuration

### Inline

```yaml
agents:
  root:
    model: openrouter/openai/gpt-5-mini
```

### Named Model

```yaml
models:
  router-gpt:
    provider: openrouter
    model: openai/gpt-5-mini
    max_tokens: 4000
    provider_opts:
      provider:
        sort: throughput
        allow_fallbacks: true
```

## Available Models

OpenRouter uses provider-prefixed model slugs such as `openai/gpt-5-mini`, `anthropic/claude-sonnet-4.5`, or `google/gemini-2.5-flash`.

Find current model names at [OpenRouter Models](https://openrouter.ai/models).

## Provider Options

OpenRouter-specific request controls live under `provider_opts`.

| Option | Description |
| ------ | ----------- |
| `x_title` | Sets the `X-Title` header for request attribution. |
| `http_referer` | Sets the `HTTP-Referer` header. |
| `models` | Optional fallback model list for routing. |
| `provider` | Provider routing controls such as `sort`, `allow_fallbacks`, `only`, and `ignore`. |
| `transforms` | OpenRouter message transforms. |
| `plugins` | OpenRouter plugins such as web search. |
| `web_search_options` | Web search context sizing. |
| `modalities` | Output modalities such as `text` or `image`. |
| `image_config` | Image generation settings such as aspect ratio and size. |

Example:

```yaml
models:
  router-web:
    provider: openrouter
    model: openai/gpt-5-mini
    provider_opts:
      x_title: docker-agent
      provider:
        sort: throughput
        allow_fallbacks: true
      plugins:
        - id: web
      web_search_options:
        search_context_size: medium
```

## Thinking Budget

OpenRouter supports the same string effort levels used by OpenAI reasoning models:

```yaml
models:
  router-thinking:
    provider: openrouter
    model: openai/o3-mini
    thinking_budget: medium # minimal | low | medium | high
```

## Embeddings

OpenRouter embedding models work anywhere docker-agent accepts an embedding model reference.

```yaml
rag:
  knowledge:
    docs:
      - ./docs
    strategies:
      - type: chunked-embeddings
        embedding_model: openrouter/openai/text-embedding-3-small
        database: ./rag/openrouter-embeddings.db
        vector_dimensions: 1536
```

<div class="callout callout-tip">
<div class="callout-title">💡 Auto-detection
</div>
  <p>If <code>OPENROUTER_API_KEY</code> is set and higher-priority cloud providers are not configured, docker-agent can auto-select OpenRouter with <code>openai/gpt-5-mini</code> as the default model.</p>

</div>