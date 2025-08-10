package main

import (
	"strings"
	"testing"
)

func TestCanonicalAndStrictNormalization(t *testing.T) {
	o := &options{normalize: normStrict}
	in := "https://EXAMPLE.com:443/a/b/123?utm_source=tw&lang=en#frag"
	norm, u, err := normalize(in, o)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if u.Host != "example.com" {
		t.Fatalf("host puny/lower fail: %s", u.Host)
	}
	if u.Fragment != "" {
		t.Fatalf("fragment not removed")
	}
	// by default we do not drop tracking params unless flags provided
	if !strings.Contains(norm, "utm_source") {
		t.Fatalf("expected to keep utm_source by default: %s", norm)
	}
}

func TestExcludeAndOnlyParams(t *testing.T) {
	o := &options{normalize: normStrict, excludeParams: []string{"utm_*", "gclid", "fbclid"}}
	n, _, err := normalize("https://example.com/a?lang=en&utm_source=x&gclid=1", o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n, "utm_source") || strings.Contains(n, "gclid") {
		t.Fatalf("tracking params not dropped: %s", n)
	}
	o2 := &options{normalize: normStrict, onlyParams: []string{"lang"}}
	n2, _, err := normalize("https://example.com/a?lang=en&a=1", o2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n2, "a=") || !strings.Contains(n2, "lang=en") {
		t.Fatalf("only params failed: %s", n2)
	}
}

func TestStructSignaturePlaceholders(t *testing.T) {
	o := &options{normalize: normStrict}
	n, u, err := normalize("https://example.com/a/b/abc123def/2024-10-05", o)
	if err != nil {
		t.Fatal(err)
	}
	_ = n
	sig, set, _ := structSignature(u, o)
	if !strings.Contains(sig, "abc{num}def") {
		t.Fatalf("digits placeholder missing in %s", sig)
	}
	if !strings.Contains(sig, "{date}") {
		t.Fatalf("date placeholder missing in %s", sig)
	}
	if _, ok := set["@lang"]; ok {
		t.Fatalf("unexpected query token present")
	}
}

func TestHybridClusteringMerges(t *testing.T) {
	o := &options{normalize: normStrict, mode: modeHybrid, simhashBits: 64, simhashShingle: 3, threshold: 0.12, domainScope: scopeRegistrable, keep: keepRichest, parallel: 2, bufferBytes: 1 << 16}
	index := newShardedIndex(4)
	st := &stats{}
	urls := []string{
		"https://EXAMPLE.com:443/a/b/123?utm_source=tw&lang=en",
		"https://example.com/a/b/456?lang=en",
		"http://example.com/a/b/789?lang=en",
		"https://example.com/a/b/123?lang=en#frag",
		"https://api.example.com/a/b/123?lang=en",
		"https://example.com/a/b/abc123def?lang=en",
		"https://example.com/a/b/2024-10-05?lang=en",
	}
	for _, s := range urls {
		ui, err := buildURLInfo(s, o, false)
		if err != nil {
			t.Fatal(err)
		}
		index.addOrMerge(ui, o, st, false)
	}
	// Expect at least 4 clusters (main bucket num/date/alpha and api subdomain)
	var total int
	for _, sh := range index.shards {
		sh.Lock()
		for _, list := range sh.buckets {
			total += len(list)
		}
		sh.Unlock()
	}
	if total < 4 || total > 8 {
		t.Fatalf("unexpected cluster count: %d", total)
	}
}
