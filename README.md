# reuniq

A fast, streaming CLI to deduplicate similar URLs from massive lists. Built for bug bounty recon and large-scale data hygiene.
Made by [R3dTr4p](https://x.com/R3dTr4p).

- Input: one URL per line (stdin or file)
- Output: one representative URL per “similarity cluster”
- Scales to millions of lines with bounded memory

## Why

Recon data is messy: tracking params, random IDs, dates, UUIDs, base64-ish blobs, and a ton of near-duplicates that waste your time. reuniq normalizes and clusters URLs so you keep one representative per group, without losing structural variety that matters for attack surface.

## TL;DR (the command I usually use)

If you don’t want to read everything, here’s a solid default that works great for big lists. Preset defaults are applied automatically (host scope). Any flags you pass override the preset.

```bash
reuniq -i big_urls.txt > unique.txt
```

JSON output (one JSON object per line):

```bash
reuniq -i big_urls.txt --json > reps.jsonl
```

To cluster across subdomains under the same eTLD+1:

```bash
reuniq --registrable-scope -i big_urls.txt > unique.txt
```

## Install

```bash
go install github.com/R3dTr4p/reuniq@latest
```

## Usage

```bash
reuniq [flags] [file]
```

Key flags:
- Input/Output
  - `-i, --input FILE` input (default: stdin)
  - `-o, --output FILE` output (default: stdout)
  - `-C, --clusters FILE` also write clusters (rep + members) to file (newline-separated blocks) — memory heavy on huge lists; may crash due to RAM
  - `--clusters-pairs FILE` stream cluster membership as `rep\tmember` lines (low memory, recommended for very large inputs)
  - `--json` emit representatives as JSON Lines to stdout (fields: `rep`, `bucket`)
- Modes
  - `-m, --mode MODE` `exact|canonical|struct|simhash|hybrid` (default: `hybrid`)
  - `-t, --threshold FLOAT` similarity threshold (mode-dependent; default 0.12 for SimHash, 0.30 for Jaccard)
- Normalization
  - `-n, --normalize LEVEL` `none|loose|strict` (default: `strict`)
  - `-x, --exclude-params LIST` drop params (wildcards supported)
  - `-X, --only-params LIST` keep only these params (overrides exclude)
  - `--no-idna` skip IDNA/punycode on hosts
  - `--http-eq-https` treat http and https as equivalent
- Scoping & performance
  - `-d, --domain-scope SCOPE` `all|registrable|host` (default: `host`)
  - `--registrable-scope` shortcut to set `registrable` (ignored if `-d` is provided)
  - `-p, --parallel INT` worker goroutines (default: `nproc`)
  - `-B, --buffer-bytes INT` line buffer (default: `1<<20`)
- Similarity tuning
  - `--simhash-bits INT` 64 or 128 (default: 64)
  - `--simhash-shingle INT` token shingle size (default: 3)
- Pre-filters (path-based)
  - `--drop-ext LIST` comma-separated extensions to drop when they are the PATH suffix (e.g., `gif,jpg,png`)
  - `--drop-b64ish` drop URLs whose PATH contains base64-ish long tokens
  - `--drop-gibberish` drop URLs whose PATH contains long alnum+ tokens (very likely random/gibberish)
- Misc
  - `-q, --quiet` suppress stats
  - `-S, --seed FILE` preload known URLs to bias cluster representatives
  - `-v, --verbose` progress updates (single-line in-place); `--progress-interval N` to change interval
  - `--version`, `--help`

## What “similar” means

- exact: byte equality after chosen normalization
- canonical: RFC-ish canonicalization + param sorting/removal → exact match on canonical form
- struct: equality of shape ignoring volatile parts
  - Path placeholders: digits → `{num}`, UUIDs → `{uuid}`, hex blobs → `{hex}`, date-like → `{date}`, base64ish → `{b64}`
  - Query compared by names; values normalized similarly
  - Jaccard on path tokens + param names
- simhash: tokenize path segments, param names and short values; compare using Hamming distance
- hybrid (default): bucket by `(registrable domain, first path segment)` then merge via:
  1) Struct signature equality (fast path)
  2) SimHash + Hamming threshold
  3) Optional canonical tie-break

## Normalization levels

- none: keep as-is
- loose:
  - lowercase scheme/host, remove default ports, strip fragment
  - collapse slashes, remove trailing slash (except root)
  - sort query params, drop empties, percent-decode unreserved
- strict (default): loose +
  - resolve `.` and `..`, IDNA/punycode host
  - normalize percent-encoding and IPv6 brackets
  - path case-sensitive; host case-insensitive
  - drop tracking params via `--exclude-params` (wildcards supported)

## Path filters (fast noise cut)

These apply only to the URL path (not query string), so `?file=logo.gif` is kept, while `/images/logo.gif` is dropped.

- `--drop-ext gif,jpg,png,css,js` → drop static/resource-y paths
- `--drop-b64ish` → drop paths containing base64-ish segments
- `--drop-gibberish` → drop paths with long alnum+ noise (e.g., `wEWCgL...`)

Use them to cheaply shrink the problem size before clustering.

## Domain scoping

- `all`: cross-domain clustering
- `registrable`: cluster within eTLD+1 (via publicsuffix)
- `host` (default): strict FQDN match

## Example: input → output

Given this input list:

```text
https://EXAMPLE.com:443/a/b/123?utm_source=tw&lang=en
https://example.com/a/b/456?lang=en
http://example.com/a/b/789?lang=en
https://example.com/a/b/123?lang=en#frag
https://api.example.com/a/b/123?lang=en
https://example.com/a/b/abc123def?lang=en
https://example.com/a/b/2024-10-05?lang=en
https://example.com/a/b/123?lang=en&a=1&a=2
https://example.com/a/b/123?lang=en&a=2&a=1
```

Running:

```bash
reuniq -m hybrid -n strict -d registrable --http-eq-https
```

Produces representatives like:

```text
https://example.com/a/b/{num}?lang=en
https://api.example.com/a/b/{num}?lang=en
https://example.com/a/b/{date}?lang=en
https://example.com/a/b/abc{num}def?lang=en
```

## Performance tips

- Prefer `-m hybrid -n strict -d host`
- Use all cores: `-p $(nproc)`
- Increase buffer for very long lines: `-B $((1<<22))` or larger
- Pre-filter aggressively on noisy corpora: `--drop-ext`, `--drop-b64ish`, `--drop-gibberish`
- For huge lists, avoid `-C`; use `--clusters-pairs` instead
- If you only need byte/canonical dedupe: `-m canonical -n strict` for max speed/min memory

## Benchmarks

Synthetic benchmark on 100k URLs (hybrid, strict, registrable, shingle=3, 64-bit SimHash):

```bash
go test -bench=. -benchmem
```

Your mileage will vary by dataset, CPU, and filtering. The pipeline is streaming and sharded; memory is driven by the number of active clusters per bucket.

## Contributing

PRs welcome. If you have real-world corpora, tuning ideas, or feature requests, please open an issue or a pull request.

## Credits

- Made by [R3dTr4p](https://x.com/R3dTr4p).
- Implementation in Go ≥1.21

Feel free to collaborate with PRs =)