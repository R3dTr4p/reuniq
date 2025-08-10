# Build a Golang CLI to deduplicate *similar* URLs from massive lists

You are an expert Go developer building a high-performance CLI for bug bounty recon. Input is **one URL per line** (stdin or a file). Output is **one representative URL per “similarity cluster”**. It should handle **millions** of lines with low memory, be fast, and give the user knobs to tune aggressiveness.

---

## Deliverables
1. A single `main.go` that compiles to `reuniq` (or `reconuniq`).
2. Clean, idiomatic Go (>=1.21), zero external deps unless truly necessary.
3. Unit tests for the core similarity engine and URL normalization.
4. Benchmarks against synthetic corpora (1e5–1e6 lines).
5. A `README.md` with usage examples and performance notes.

---

## CLI design
reuniq [flags] [file]

Flags:
-i, --input FILE Input file (default: stdin)
-o, --output FILE Output file (default: stdout)
-m, --mode MODE Similarity mode: exact|canonical|struct|simhash|hybrid (default: hybrid)
-t, --threshold FLOAT Similarity threshold (mode-dependent, default 0.12 for simhash/Hamming, 0.3 Jaccard)
-n, --normalize LEVEL Normalization: none|loose|strict (default: strict)
-k, --keep POLICY Which URL to keep per cluster: first|shortest|longest|richest|lexi (default: richest)
-d, --domain-scope SCOPE all|registrable|host (default: registrable)
-p, --parallel INT Worker goroutines (default: number of CPUs)
-B, --buffer-bytes INT Line buffer size (default: 1<<20)
-x, --exclude-params LIST Comma-separated params to drop (e.g., utm_*, gclid, fbclid)
-X, --only-params LIST Only keep these params (whitelist); overrides exclude
-q, --quiet Suppress stats to stderr
-C, --clusters FILE Also write clusters (rep + members) to FILE (newline-separated blocks)
-S, --seed FILE Preload known URLs to bias cluster representatives
--simhash-bits INT 64 or 128 (default: 64)
--simhash-shingle INT Token shingle size (default: 3)
--struct-weight FLOAT Weight of structural vs query similarity in hybrid (default: 0.6)
--no-idna Do not punycode/IDNA-normalize hostnames
--http-eq-https Treat http and https as equivalent for similarity scoring
--version
--help

markdown
Copia
Modifica

---

## What “similar” means (modes)
- **exact**: Byte-for-byte equality after chosen normalization.
- **canonical**: RFC-ish canonicalization + param sorting/removal → exact match on canonical form.
- **struct**: Equality of *shape* ignoring volatile parts.
  - `/api/v1/users/123/profile` ~ `/api/v1/users/456/profile` (digits → `{num}`)
  - UUIDs → `{uuid}`, hex blobs → `{hex}`, datestamps → `{date}`, long Base64-ish → `{b64}`
  - Query params compared by names; values normalized like above
  - Jaccard on path tokens + param names
- **simhash**: Tokenize path segments, param names, and short values; compute SimHash (64/128). URLs within a Hamming radius form a cluster.
- **hybrid (default)**: Bucket by (registrable domain, first path segment) → within bucket apply:
  1) Struct signature equality for cheap merges,
  2) SimHash + Hamming distance for fuzzy near-duplicates,
  3) Optional canonical tie-break.

---

## Normalization levels
- **none**: keep as-is.
- **loose**:
  - Lowercase scheme/host
  - Remove default ports (80 for http, 443 for https)
  - Strip fragments
  - Sort query params; drop empties; percent-decode unreserved
  - Collapse multiple slashes; remove trailing slash except root
- **strict** (default):
  - All “loose”, plus:
  - Resolve `.` and `..`
  - IDNA/punycode host (unless `--no-idna`)
  - Percent-encode reserved properly and decode unreserved
  - Normalize IPv6 brackets
  - Normalize case sensitivity: path case-sensitive, host case-insensitive
  - Remove session/tracking params via `--exclude-params` (wildcards `*` supported)

---

## Heuristics for bug bounty noise
Normalize volatile elements in **struct**/**hybrid**:
- Integers: `\d+` → `{num}`
- UUIDs: `[0-9a-fA-F-]{36}` → `{uuid}`
- Hex IDs: `\b[0-9a-fA-F]{8,}\b` → `{hex}`
- Base64ish: `[-_A-Za-z0-9]{16,}={0,2}` → `{b64}`
- Datelike: `\b20\d{2}[-/]\d{1,2}[-/]\d{1,2}\b` → `{date}`
- Timestamps: `\b1[5-9]\d{8,}\b` → `{ts}`
- Query params commonly noisy: `utm_*`, `gclid`, `fbclid`, `mc_eid`, `ref`, `session`, `sid`, `phpsessid`, `JSESSIONID`, `_ga`, `_gid`

---

## Clustering strategy (scales to millions)
1. **Streaming ingestion** with `bufio.Reader` (avoid default Scanner limits).
2. **Bucket first** by `(scope(host), firstPathSegment)` to localize similarity checks.
3. **Representative selection** (`--keep` policy):
   - `first` | `shortest` | `longest` | `richest` | `lexi`
4. **Near-dup detection**:
   - Build struct signature → equal hash ⇒ merge.
   - Else, compute SimHash → if within threshold Hamming distance ⇒ merge.
5. **Memory safety**:
   - Only store reps + fingerprints.
   - Sharded maps with mutexes for concurrency.

---

## Domain scoping
- `all`: cross-domain clustering
- `registrable`: cluster within eTLD+1
- `host`: strict FQDN match

Use [publicsuffix](https://pkg.go.dev/golang.org/x/net/publicsuffix) for parsing.

---

## Output modes
- Default: reps only.
- `-C clusters.txt`: reps + members in grouped blocks.
- Stats to stderr unless `--quiet`.

---

## Edge cases to test
- HTTP/HTTPS equivalence
- Default ports
- Param order/dupes
- Percent-encoding differences
- Mixed case paths
- Dot-segment resolution
- IPv6 hosts
- IDNA domains
- Empty lines & comments
- Fragment-only differences
- Very long lines

---

## Implementation sketch
- `URLInfo` struct with raw, normalized, scope, bucket, struct hash, simhash, etc.
- Separate files:
  - `normalize.go`
  - `signature.go`
  - `cluster.go`
  - `cmd/main.go`

---

## Performance targets
- 1M lines, hybrid mode, 8 cores: < 30–60s, < 1.5 GB RAM
- Streaming: constant memory with bounded bucket state

---

## Example usage
cat wayback.txt | reuniq -m hybrid -n strict -d registrable -x "utm_*,gclid,fbclid,_ga" -p 8 > unique.txt
reuniq -i wayback.txt -C clusters.txt --http-eq-https --threshold 0.12

yaml
Copia
Modifica

---

## Test vectors
Input:
https://EXAMPLE.com:443/a/b/123?utm_source=tw&lang=en
https://example.com/a/b/456?lang=en
http://example.com/a/b/789?lang=en
https://example.com/a/b/123?lang=en#frag
https://api.example.com/a/b/123?lang=en
https://example.com/a/b/abc123def?lang=en
https://example.com/a/b/2024-10-05?lang=en
https://example.com/a/b/123?lang=en&a=1&a=2
https://example.com/a/b/123?lang=en&a=2&a=1

scss
Copia
Modifica
Expected (hybrid, strict, registrable, http-eq-https):
https://example.com/a/b/{num}?lang=en
https://api.example.com/a/b/{num}?lang=en
https://example.com/a/b/{date}?lang=en
https://example.com/a/b/abc{num}def?lang=en

yaml
Copia
Modifica

---

## Nice-to-haves
- `--emit-placeholders`
- `--min-param-count`
- `--max-path-len`
- `--bloom`
- `--progress`
- `--json`
- `--seed` to bias reps

---

## Acceptance criteria
- ≥70% reduction on noisy inputs without losing structural variety
- No panics on malformed URLs (skip + count)
- Deterministic output for same input/flags

---

Implementation: Initial CLI, tests, and benchmarks added in `main.go`, `main_test.go`, and `bench_test.go`. README included.