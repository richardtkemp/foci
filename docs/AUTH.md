# Authentication Setup

Foci uses two separate Anthropic tokens with different roles.

## The Two Tokens

### `anthropic.token` — Conversations (required)

OAuth setup-token from a Claude Max subscription. Used for the Messages API (all conversations and model interactions).

- Token format: `sk-ant-oat01-...`
- Lifetime: 1 year
- Billing: draws from Max plan subscription quota (mana)
- Source: `claude setup-token` command

### `anthropic.admin_key` — Admin/Usage (optional)

Console API key from console.anthropic.com. Used for usage/mana queries and token counting. This is a standard API key, not an OAuth token.

- Token format: `sk-ant-api03-...`
- Billing: prepaid API credits (separate from Max plan)
- Source: console.anthropic.com > Settings > API Keys
- Used for: `/mana` command, `/context` token counts, usage warning thresholds

**Why separate?** The setup-token authenticates against the Max subscription for conversations. The console API key authenticates against the admin/billing API for usage queries and the free token counting endpoint. These are different authentication contexts — the setup-token may not have access to admin endpoints, and the console key should not be used for conversations (it would bill to prepaid credits instead of the Max plan).

## Setup

### Step 1: Get the conversation token

Run `claude setup-token` in a terminal. This opens a browser for OAuth authentication and outputs a token.

```
$ claude setup-token
# Browser opens → authenticate → token printed
sk-ant-oat01-...
```

### Step 2: Get the admin API key

1. Go to [console.anthropic.com](https://console.anthropic.com)
2. Navigate to Settings > API Keys
3. Create a new key (any name, e.g. "foci-admin")
4. Copy the key (`sk-ant-api03-...`)

### Step 3: Store both in secrets.toml

```toml
[anthropic]
token = "sk-ant-oat01-..."
admin_key = "sk-ant-api03-..."
```

Alternatively, if using `credentials_file` (fallback for the main token):

```toml
[anthropic]
admin_key = "sk-ant-api03-..."
```

With `credentials_file` configured in `foci.toml`, the main token is read from `~/.claude/.credentials.json` automatically. Only `admin_key` needs to be in secrets.toml.

## What Happens Without admin_key

If `admin_key` is not configured:

- A warning is logged at startup
- Mana/usage checks fall back to the main token (may not work with setup-token auth)
- Token counting falls back to the main token (may not work with setup-token auth)
- All conversations and model interactions work normally

The main token is the only required credential. Everything else degrades gracefully.
