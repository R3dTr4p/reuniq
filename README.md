# reuniq

A high-performance Go CLI to deduplicate similar URLs from massive lists for bug bounty recon.

- Input: one URL per line (stdin or a file)
- Output: one representative URL per “similarity cluster”
- Scales to millions of lines with low memory

## Install

```bash
go install ./...
```

Or build locally:

```bash
go build -o reuniq
```

## Usage

```bash
reuniq [flags] [file]
```

Flags:
- `-i, --input FILE` Input file (default: stdin)
- `-o, --output FILE` Output file (default: stdout)
- `-m, --mode MODE` Similarity mode: exact|canonical|struct|simhash|hybrid (default: hybrid)
- `-t, --threshold FLOAT` Similarity threshold (mode-dependent, default 0.12 for simhash/Hamming, 0.3 Jaccard)
- `-n, --normalize LEVEL` Normalization: none|loose|strict (default: strict)
- `-k, --keep POLICY` Which URL to keep per cluster: first|shortest|longest|richest|lexi (default: richest)
- `-d, --domain-scope SCOPE` all|registrable|host (default: registrable)
- `-p, --parallel INT` Worker goroutines (default: number of CPUs)
- `-B, --buffer-bytes INT` Line buffer size (default: 1<<20)
- `-x, --exclude-params LIST` Comma-separated params to drop (e.g., utm_*, gclid, fbclid)
- `-X, --only-params LIST` Only keep these params (whitelist); overrides exclude
- `-q, --quiet` Suppress stats to stderr
- `-C, --clusters FILE` Also write clusters (rep + members) to FILE (newline-separated blocks)
- `-S, --seed FILE` Preload known URLs to bias cluster representatives
- `--simhash-bits INT` 64 or 128 (default: 64)
- `--simhash-shingle INT` Token shingle size (default: 3)
- `--struct-weight FLOAT` Weight of structural vs query similarity in hybrid (default: 0.6)
- `--no-idna` Do not punycode/IDNA-normalize hostnames
- `--http-eq-https` Treat http and https as equivalent for similarity scoring
- `--version`
- `--help`

## Examples

```bash
cat wayback.txt | reuniq -m hybrid -n strict -d registrable -x "utm_*,gclid,fbclid,_ga" -p 8 > unique.txt
reuniq -i wayback.txt -C clusters.txt --http-eq-https --threshold 0.12
```

## Notes
- Hybrid mode buckets by (registrable domain, first path segment) and merges via struct signature equality and SimHash + Hamming distance.
- In struct mode, shape-like placeholders identify near-duplicates: digits → {num}, UUIDs → {uuid}, hex → {hex}, date-like → {date}, base64ish → {b64}.
- Strict normalization resolves dot segments, IDNA-normalizes host, drops tracking params, sorts query params, and removes fragments.

## Performance
- Targets: 1M lines, hybrid, 8 cores: < 30–60s, < 1.5 GB RAM
- Streaming ingestion with sharded buckets and bounded state per bucket.

## Testing

```bash
go test ./... -v
```

## Benchmarks

```bash
go test -bench=. -benchmem
``` 