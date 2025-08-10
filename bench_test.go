package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func BenchmarkPipeline_100k(b *testing.B) {
	// build ~100k synthetic URLs
	N := 100_000
	buf := &bytes.Buffer{}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < N; i++ {
		d := i % 1000
		domain := fmt.Sprintf("sub%d.example%d.com", d%7, d%13)
		path := fmt.Sprintf("/api/v1/users/%d/profile", rng.Intn(1_000_000))
		fmt.Fprintf(buf, "https://%s%s?lang=en&a=%d\n", domain, path, rng.Intn(100))
	}
	o := &options{normalize: normStrict, mode: modeHybrid, simhashBits: 64, simhashShingle: 3, threshold: 0.12, domainScope: scopeRegistrable, keep: keepRichest, parallel: 4, bufferBytes: 1 << 20}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = process(o, bytes.NewReader(buf.Bytes()), ioDiscard{}, nil)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
