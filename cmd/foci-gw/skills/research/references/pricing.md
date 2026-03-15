# OpenRouter Perplexity Pricing

## Per-Model Pricing

| Model | Input | Output | Context |
|-------|-------|--------|---------|
| perplexity/sonar | $2/M tokens | $8/M tokens | 128K |
| perplexity/sonar-deep-research | $2/M tokens | $8/M tokens | 128K |

## Cost Examples

- Basic query (500 input + 200 output tokens): ~$0.003
- Longer research (2000 input + 1500 output tokens): ~$0.016
- Deep research (3000 input + 3000 output tokens): ~$0.030

## Why OpenRouter?

- No separate Perplexity API account needed
- Centralized billing alongside other models
- Same pricing as direct Perplexity API
- Integrates seamlessly with existing OpenRouter setup
