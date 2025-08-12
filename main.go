package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const banner = `  _____                  _       
 |  __ \                (_)      
 | |__) |___ _   _ _ __  _  __ _ 
 |  _  // _ \ | | | '_ \| |/ _' |
 | | \ \  __/ |_| | | | | | (_| |
 |_|  \_\___|\__,_|_| |_|_|\__, |
                              | |
                              |_| 
----------------------------------------
Made by: R3dTr4p
`

const (
	defaultThresholdSimhash = 0.12
	defaultThresholdStruct  = 0.30
)

type modeType string

type normalizeLevel string

type keepPolicy string

type domainScope string

const (
	modeExact     modeType = "exact"
	modeCanonical modeType = "canonical"
	modeStruct    modeType = "struct"
	modeSimhash   modeType = "simhash"
	modeHybrid    modeType = "hybrid"

	normNone   normalizeLevel = "none"
	normLoose  normalizeLevel = "loose"
	normStrict normalizeLevel = "strict"

	keepFirst    keepPolicy = "first"
	keepShortest keepPolicy = "shortest"
	keepLongest  keepPolicy = "longest"
	keepRichest  keepPolicy = "richest"
	keepLexi     keepPolicy = "lexi"

	scopeAll         domainScope = "all"
	scopeRegistrable domainScope = "registrable"
	scopeHost        domainScope = "host"
)

type options struct {
	inputPath       string
	outputPath      string
	clustersPath    string
	seedPath        string
	mode            modeType
	threshold       float64
	normalize       normalizeLevel
	keep            keepPolicy
	domainScope     domainScope
	parallel        int
	bufferBytes     int
	excludeParams   []string
	onlyParams      []string
	quiet           bool
	simhashBits     int
	simhashShingle  int
	structWeight    float64
	noIDNA          bool
	httpEqHttps     bool
	version         bool
	verbose         bool
	dropExts        []string
	dropB64ish      bool
	dropGibberish   bool
	presetClean     bool
	progressEveryS  int
	registrableFlag bool
}

type stats struct {
	linesRead    int64
	linesParsed  int64
	linesSkipped int64
	clusters     int64
	merged       int64
}

type urlInfo struct {
	raw        string
	norm       string
	canonKey   string
	scope      string
	bucket     string
	structSig  string
	structSet  map[string]struct{}
	sim64      uint64
	sim128Hi   uint64
	sim128Lo   uint64
	pathDepth  int
	paramCount int
	isSeed     bool
}

type cluster struct {
	rep     *urlInfo
	members []*urlInfo // only if clusters output requested; otherwise nil to save memory
}

type shard struct {
	sync.Mutex
	buckets map[string]*bucketState // key: bucket string
}

type shardedIndex struct {
	shards []*shard
}

type bucketState struct {
	clusters []*cluster
	byStruct map[string]*cluster
}

func newShardedIndex(numShards int) *shardedIndex {
	if numShards < 1 {
		numShards = 1
	}
	s := &shardedIndex{shards: make([]*shard, numShards)}
	for i := 0; i < numShards; i++ {
		s.shards[i] = &shard{buckets: make(map[string]*bucketState)}
	}
	return s
}

func (s *shardedIndex) getShard(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := int(h.Sum32()) % len(s.shards)
	if idx < 0 {
		idx = -idx
	}
	return s.shards[idx]
}

// addOrMerge inserts the urlInfo into the index, creating or merging clusters according to options.
// Returns true if merged into existing cluster; false if created new cluster.
func (s *shardedIndex) addOrMerge(u *urlInfo, o *options, st *stats, wantMembers bool) bool {
	sh := s.getShard(u.bucket)
	sh.Lock()
	defer sh.Unlock()

	bs := sh.buckets[u.bucket]
	if bs == nil {
		bs = &bucketState{byStruct: make(map[string]*cluster)}
		sh.buckets[u.bucket] = bs
	}

	// Try merge into existing clusters in the same bucket
	// Fast path: exact structSig match (only in modes where structure equality is a criterion)
	if (o.mode == modeHybrid || o.mode == modeStruct) && u.structSig != "" {
		if c, ok := bs.byStruct[u.structSig]; ok {
			atomic.AddInt64(&st.merged, 1)
			c.rep = chooseRepresentative(c.rep, u, o.keep)
			if wantMembers {
				c.members = append(c.members, u)
			}
			return true
		}
	}
	// Fallback: scan clusters for simhash/canonical proximity when enabled
	for _, c := range bs.clusters {
		if shouldMerge(u, c.rep, o) {
			atomic.AddInt64(&st.merged, 1)
			c.rep = chooseRepresentative(c.rep, u, o.keep)
			if wantMembers {
				c.members = append(c.members, u)
			}
			// If structSig match just occurred via shouldMerge (unlikely since map missed), index it now
			if (o.mode == modeHybrid || o.mode == modeStruct) && u.structSig == c.rep.structSig {
				bs.byStruct[u.structSig] = c
			}
			return true
		}
	}
	// No merge found, create new cluster
	newc := &cluster{rep: u}
	if wantMembers {
		newc.members = append(newc.members, u)
	}
	bs.clusters = append(bs.clusters, newc)
	// Index by struct signature for fast future merges
	if (o.mode == modeHybrid || o.mode == modeStruct) && u.structSig != "" {
		bs.byStruct[u.structSig] = newc
	}
	atomic.AddInt64(&st.clusters, 1)
	return false
}

func chooseRepresentative(a, b *urlInfo, policy keepPolicy) *urlInfo {
	switch policy {
	case keepFirst:
		return a
	case keepShortest:
		if len(b.norm) < len(a.norm) {
			return b
		}
		return a
	case keepLongest:
		if len(b.norm) > len(a.norm) {
			return b
		}
		return a
	case keepLexi:
		if b.norm < a.norm {
			return b
		}
		return a
	case keepRichest:
		// Prefer seeded rep
		if b.isSeed && !a.isSeed {
			return b
		}
		if a.isSeed && !b.isSeed {
			return a
		}
		// Score by path depth, param count, then length as tie-breaker
		scoreA := a.pathDepth*3 + a.paramCount*2
		scoreB := b.pathDepth*3 + b.paramCount*2
		if scoreB > scoreA {
			return b
		}
		if scoreA > scoreB {
			return a
		}
		if len(b.norm) > len(a.norm) {
			return b
		}
		return a
	default:
		return a
	}
}

func canonicalComparable(s string, httpEq bool) string {
	c := canonicalForm(s)
	if !httpEq {
		return c
	}
	if strings.HasPrefix(c, "http://") {
		return c[len("http://"):]
	}
	if strings.HasPrefix(c, "https://") {
		return c[len("https://"):]
	}
	return c
}

func shouldMerge(a *urlInfo, rep *urlInfo, o *options) bool {
	switch o.mode {
	case modeExact:
		return a.norm == rep.norm
	case modeCanonical:
		return a.canonKey == rep.canonKey
	case modeStruct:
		// Jaccard similarity on struct token sets
		aSet := a.structSet
		rSet := rep.structSet
		j := jaccard(aSet, rSet)
		return j >= o.threshold
	case modeSimhash:
		return simhashClose(a, rep, o)
	case modeHybrid:
		if a.structSig == rep.structSig {
			return true
		}
		if simhashClose(a, rep, o) {
			return true
		}
		// optional canonical tie-break
		return a.canonKey == rep.canonKey
	default:
		return false
	}
}

func simhashClose(a, b *urlInfo, o *options) bool {
	if o.simhashBits == 128 {
		d := hamming128(a.sim128Hi, a.sim128Lo, b.sim128Hi, b.sim128Lo)
		max := int(float64(128) * o.threshold)
		return d <= max
	}
	d := hamming64(a.sim64, b.sim64)
	max := int(float64(64) * o.threshold)
	return d <= max
}

func canonicalForm(s string) string {
	// Use a simple canonicalization: lowercase scheme/host, remove default port, sort params
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	h, p, err := net.SplitHostPort(host)
	if err == nil {
		if (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
			host = h
		}
	}
	u.Host = host
	u.Fragment = ""
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	newQ := url.Values{}
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			if v == "" {
				continue
			}
			newQ.Add(k, v)
		}
	}
	u.RawQuery = newQ.Encode()
	p2 := path.Clean(u.Path)
	if !strings.HasPrefix(p2, "/") {
		p2 = "/" + p2
	}
	if p2 != "/" && strings.HasSuffix(p2, "/") {
		p2 = strings.TrimRight(p2, "/")
	}
	u.Path = p2
	return u.String()
}

// Normalization
func normalize(raw string, o *options) (string, *url.URL, error) {
	if o.normalize == normNone {
		u, err := url.Parse(strings.TrimSpace(raw))
		return raw, u, err
	}

	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", nil, err
	}

	if o.normalize == normLoose || o.normalize == normStrict {
		u.Scheme = strings.ToLower(u.Scheme)
		if !o.noIDNA {
			ascii, err := idna.ToASCII(u.Hostname())
			if err == nil && ascii != "" {
				if u.Port() != "" {
					u.Host = net.JoinHostPort(strings.ToLower(ascii), u.Port())
				} else {
					u.Host = strings.ToLower(ascii)
				}
			} else {
				u.Host = strings.ToLower(u.Host)
			}
		} else {
			u.Host = strings.ToLower(u.Host)
		}

		// Remove default ports
		host := u.Host
		if p := u.Port(); p != "" {
			if (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
				host = u.Hostname()
			}
		}
		u.Host = host

		// Drop fragment
		u.Fragment = ""

		// Clean path
		p := u.Path
		if p == "" {
			p = "/"
		}
		p = strings.ReplaceAll(p, "//", "/")
		p = path.Clean(p)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if p != "/" && strings.HasSuffix(p, "/") {
			p = strings.TrimRight(p, "/")
		}
		u.Path = p

		// Query params handling: only-params overrides exclude-params
		q := u.Query()
		if len(o.onlyParams) > 0 {
			filtered := url.Values{}
			for _, k := range sortedKeys(q) {
				if matchAny(k, o.onlyParams) {
					vals := q[k]
					sort.Strings(vals)
					for _, v := range vals {
						if v == "" { // drop empties
							continue
						}
						filtered.Add(k, v)
					}
				}
			}
			u.RawQuery = filtered.Encode()
		} else {
			if len(o.excludeParams) > 0 {
				for k := range q {
					if matchAny(k, o.excludeParams) {
						q.Del(k)
					}
				}
			}
			// drop empties, sort keys and values
			keys := sortedKeys(q)
			newQ := url.Values{}
			for _, k := range keys {
				vals := q[k]
				sort.Strings(vals)
				for _, v := range vals {
					if v == "" {
						continue
					}
					newQ.Add(k, v)
				}
			}
			u.RawQuery = newQ.Encode()
		}

		// Percent-encode reserved properly, decode unreserved (best-effort via url.URL)
	}

	// Strict: resolve dot segments already via path.Clean above

	return u.String(), u, nil
}

func sortedKeys(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func matchAny(s string, patterns []string) bool {
	for _, pat := range patterns {
		ok, err := path.Match(pat, s)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// Struct signature and tokenization
var (
	reDigits = regexp.MustCompile(`\d+`)
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// Treat only long hex-like blobs as placeholders to avoid over-matching small alphanumerics like abc123def
	reHexLong       = regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`)
	reBase64ish     = regexp.MustCompile(`[-_A-Za-z0-9]{16,}={0,2}`)
	reAlnumPlusLong = regexp.MustCompile(`[A-Za-z0-9+]{16,}(?:={0,2})?`)
	reDateLike      = regexp.MustCompile(`\b20\d{2}[-/]\d{1,2}[-/]\d{1,2}\b`)
	reTS            = regexp.MustCompile(`\b1[5-9]\d{8,}\b`)
)

func buildURLInfo(raw string, o *options, seeded bool) (*urlInfo, error) {
	norm, u, err := normalize(raw, o)
	if err != nil {
		return nil, err
	}
	// Compute scope
	host := u.Hostname()
	var scope string
	switch o.domainScope {
	case scopeAll:
		scope = "*"
	case scopeHost:
		scope = strings.ToLower(host)
	case scopeRegistrable:
		etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
		if err != nil {
			// fallback to host
			scope = strings.ToLower(host)
		} else {
			scope = strings.ToLower(etld1)
		}
	}

	firstSeg := firstPathSegment(u.Path)
	bucket := scope + "|" + firstSeg

	// struct signature and optional set
	structSig, structSet, depth := structSignature(u, o)
	if o.mode != modeStruct {
		structSet = nil
	}

	// Precompute canonical comparison key once
	canonKey := canonicalComparable(norm, o.httpEqHttps)

	// Simhash only when needed
	var sh64, hi, lo uint64
	if o.mode == modeSimhash || o.mode == modeHybrid {
		sh64, hi, lo = computeSimhash(u, o)
	}

	paramCount := 0
	if q := u.Query(); len(q) > 0 {
		for _, v := range q {
			paramCount += len(v)
		}
	}

	return &urlInfo{
		raw:        raw,
		norm:       norm,
		canonKey:   canonKey,
		scope:      scope,
		bucket:     bucket,
		structSig:  structSig,
		structSet:  structSet,
		sim64:      sh64,
		sim128Hi:   hi,
		sim128Lo:   lo,
		pathDepth:  depth,
		paramCount: paramCount,
		isSeed:     seeded,
	}, nil
}

func firstPathSegment(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	if strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	idx := strings.IndexByte(p, '/')
	if idx == -1 {
		return p
	}
	return p[:idx]
}

func structSignature(u *url.URL, o *options) (string, map[string]struct{}, int) {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	depth := 0
	var buf bytes.Buffer
	if u.Path == "/" || u.Path == "" {
		// empty
	} else {
		for i, s := range segs {
			if s == "" {
				continue
			}
			depth++
			if i > 0 {
				buf.WriteByte('/')
			}
			ns := applyPlaceholders(s)
			buf.WriteString(ns)
		}
	}
	// Query param names, ignore values for struct signature
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		buf.WriteByte('?')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte('&')
			}
			buf.WriteByte('@')
			buf.WriteString(k)
		}
	}
	// build set for Jaccard: tokens include path segments after placeholder and param names
	set := make(map[string]struct{}, len(segs)+len(keys))
	for _, s := range segs {
		if s == "" {
			continue
		}
		set[applyPlaceholders(s)] = struct{}{}
	}
	for _, k := range keys {
		set["@"+k] = struct{}{}
	}
	return buf.String(), set, depth
}

func applyPlaceholders(s string) string {
	s = reUUID.ReplaceAllString(s, "{uuid}")
	s = reDateLike.ReplaceAllString(s, "{date}")
	s = reTS.ReplaceAllString(s, "{ts}")
	s = reBase64ish.ReplaceAllString(s, "{b64}")
	s = reHexLong.ReplaceAllString(s, "{hex}")
	s = reDigits.ReplaceAllString(s, "{num}")
	return s
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Simhash
func computeSimhash(u *url.URL, o *options) (uint64, uint64, uint64) {
	// tokens: path segments, param names, short values (<=16)
	tokens := make([]string, 0, 16)
	for _, seg := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if seg == "" {
			continue
		}
		tokens = append(tokens, applyPlaceholders(strings.ToLower(seg)))
	}
	q := u.Query()
	for k, vals := range q {
		lk := strings.ToLower(k)
		tokens = append(tokens, "@"+lk)
		for _, v := range vals {
			lv := strings.ToLower(v)
			if len(lv) > 0 && len(lv) <= 16 {
				tokens = append(tokens, lk+"="+lv)
			}
		}
	}

	// shingles
	if o.simhashShingle > 1 {
		shingled := make([]string, 0, len(tokens))
		for i := 0; i+o.simhashShingle <= len(tokens); i++ {
			shingled = append(shingled, strings.Join(tokens[i:i+o.simhashShingle], "/"))
		}
		if len(shingled) > 0 {
			tokens = shingled
		}
	}

	v := make([]int, 64)
	var v2 []int
	if o.simhashBits == 128 {
		v2 = make([]int, 64)
	}
	for _, t := range tokens {
		h := fnv64a(t)
		for i := 0; i < 64; i++ {
			if (h>>uint(i))&1 == 1 {
				v[i]++
			} else {
				v[i]--
			}
		}
		if v2 != nil {
			// second hash with salt via sha1
			sh := sha1.Sum([]byte("s:" + t))
			var h2 uint64
			for i := 0; i < 8; i++ {
				h2 = (h2 << 8) | uint64(sh[i])
			}
			for i := 0; i < 64; i++ {
				if (h2>>uint(i))&1 == 1 {
					v2[i]++
				} else {
					v2[i]--
				}
			}
		}
	}
	var out1 uint64
	for i := 0; i < 64; i++ {
		if v[i] >= 0 {
			out1 |= 1 << uint(i)
		}
	}
	if v2 == nil {
		return out1, 0, 0
	}
	var out2 uint64
	for i := 0; i < 64; i++ {
		if v2[i] >= 0 {
			out2 |= 1 << uint(i)
		}
	}
	return out1, out1, out2
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func hamming64(a, b uint64) int {
	return bitsCount64(a ^ b)
}

func hamming128(ahi, alo, bhi, blo uint64) int {
	return bitsCount64(ahi^bhi) + bitsCount64(alo^blo)
}

func bitsCount64(x uint64) int {
	// builtin in Go 1.21: bits.OnesCount64
	// Avoid importing math/bits to keep file self-contained
	count := 0
	for x != 0 {
		x &= x - 1
		count++
	}
	return count
}

// I/O and pipeline
func readLines(r io.Reader, bufSize int, out chan<- string) error {
	br := bufio.NewReaderSize(r, bufSize)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// trim trailing CR/LF/spaces
			l := strings.TrimRight(string(line), "\r\n")
			out <- l
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func process(o *options, input io.Reader, repsOut io.Writer, clustersOut io.Writer) (stats, error) {
	st := stats{}
	wantMembers := clustersOut != nil
	index := newShardedIndex(runtime.GOMAXPROCS(0) * 2)

	lines := make(chan string, o.parallel*4)
	var wg sync.WaitGroup

	// periodic progress logger
	var progressDone chan struct{}
	if o.verbose && !o.quiet {
		progressDone = make(chan struct{})
		go func() {
			interval := time.Duration(o.progressEveryS)
			if interval <= 0 {
				interval = 1
			}
			t := time.NewTicker(interval * time.Second)
			defer t.Stop()
			prevLen := 0
			for {
				select {
				case <-progressDone:
					// finish the in-place line
					fmt.Fprint(os.Stderr, "\n")
					return
				case <-t.C:
					msg := fmt.Sprintf("progress lines=%d parsed=%d skipped=%d clusters=%d merged=%d",
						atomic.LoadInt64(&st.linesRead),
						atomic.LoadInt64(&st.linesParsed),
						atomic.LoadInt64(&st.linesSkipped),
						atomic.LoadInt64(&st.clusters),
						atomic.LoadInt64(&st.merged))
					// overwrite same line using carriage return
					padding := 0
					if prevLen > len(msg) {
						padding = prevLen - len(msg)
					}
					if padding > 0 {
						fmt.Fprintf(os.Stderr, "\r%s%s", msg, strings.Repeat(" ", padding))
					} else {
						fmt.Fprintf(os.Stderr, "\r%s", msg)
					}
					prevLen = len(msg)
				}
			}
		}()
	}

	worker := func() {
		defer wg.Done()
		for l := range lines {
			atomic.AddInt64(&st.linesRead, 1)
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			ui, err := buildURLInfo(l, o, false)
			if err != nil {
				atomic.AddInt64(&st.linesSkipped, 1)
				continue
			}
			// Drop filters
			if shouldDropURL(ui, o) {
				atomic.AddInt64(&st.linesSkipped, 1)
				continue
			}
			atomic.AddInt64(&st.linesParsed, 1)
			index.addOrMerge(ui, o, &st, wantMembers)
		}
	}

	wg.Add(o.parallel)
	for i := 0; i < o.parallel; i++ {
		go worker()
	}

	// Optionally load seed first
	if o.seedPath != "" {
		f, err := os.Open(o.seedPath)
		if err == nil {
			_ = feedReader(f, o, index, &st, wantMembers, true)
			_ = f.Close()
		}
	}

	// Feed main input
	go func() {
		_ = readLines(input, o.bufferBytes, lines)
		close(lines)
	}()

	wg.Wait()

	// Emit reps
	if err := emitOutputs(index, repsOut, clustersOut); err != nil {
		if progressDone != nil {
			close(progressDone)
		}
		return st, err
	}

	if progressDone != nil {
		close(progressDone)
	}
	return st, nil
}

func shouldDropURL(ui *urlInfo, o *options) bool {
	// Drop by path extension (case-insensitive), applied to the path only (not query)
	if len(o.dropExts) > 0 {
		lp := strings.ToLower(uiPath(ui))
		for _, ext := range o.dropExts {
			e := strings.ToLower(ext)
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			if strings.HasSuffix(lp, e) {
				return true
			}
		}
	}
	// Drop b64ish or gibberish-like segments in path if opted-in
	if o.dropB64ish || o.dropGibberish {
		segs := strings.Split(strings.Trim(uiPath(ui), "/"), "/")
		for _, s := range segs {
			if s == "" {
				continue
			}
			if o.dropB64ish && reBase64ish.MatchString(s) {
				return true
			}
			if o.dropGibberish && reAlnumPlusLong.MatchString(s) {
				return true
			}
		}
	}
	return false
}

func uiPath(ui *urlInfo) string {
	// ui.norm is a full URL string; parse path cheaply
	// We also stored structSig and pathDepth, but to get the raw path we reparse minimal
	// For performance, attempt to slice out path by looking for '://' then next '/'
	s := ui.norm
	// find scheme sep
	i := strings.Index(s, "://")
	if i == -1 {
		// best effort
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return u.Path
	}
	// find first '/'
	j := strings.IndexByte(s[i+3:], '/')
	if j == -1 {
		return "/"
	}
	// path starts at i+3+j
	k := i + 3 + j
	// up to '?' or end
	if q := strings.IndexByte(s[k:], '?'); q != -1 {
		return s[k : k+q]
	}
	return s[k:]
}

func feedReader(r io.Reader, o *options, index *shardedIndex, st *stats, wantMembers bool, seeded bool) error {
	br := bufio.NewReaderSize(r, o.bufferBytes)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			l := strings.TrimRight(string(line), "\r\n")
			if l != "" && !strings.HasPrefix(l, "#") {
				ui, e := buildURLInfo(l, o, seeded)
				if e == nil {
					if !shouldDropURL(ui, o) {
						index.addOrMerge(ui, o, st, wantMembers)
						atomic.AddInt64(&st.linesParsed, 1)
					} else {
						atomic.AddInt64(&st.linesSkipped, 1)
					}
				} else {
					atomic.AddInt64(&st.linesSkipped, 1)
				}
				atomic.AddInt64(&st.linesRead, 1)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func emitOutputs(index *shardedIndex, repsOut io.Writer, clustersOut io.Writer) error {
	bufReps := bufio.NewWriter(repsOut)
	var bufClus *bufio.Writer
	if clustersOut != nil {
		bufClus = bufio.NewWriter(clustersOut)
	}
	for _, sh := range index.shards {
		sh.Lock()
		for _, bs := range sh.buckets {
			for _, c := range bs.clusters {
				if _, err := bufReps.WriteString(c.rep.norm + "\n"); err != nil {
					sh.Unlock()
					return err
				}
				if bufClus != nil {
					// write block: rep then each member, then blank line
					if _, err := bufClus.WriteString(c.rep.norm + "\n"); err != nil {
						sh.Unlock()
						return err
					}
					if len(c.members) > 0 {
						for _, m := range c.members {
							if _, err := bufClus.WriteString(m.norm + "\n"); err != nil {
								sh.Unlock()
								return err
							}
						}
					}
					if _, err := bufClus.WriteString("\n"); err != nil {
						sh.Unlock()
						return err
					}
				}
			}
		}
		sh.Unlock()
	}
	if err := bufReps.Flush(); err != nil {
		return err
	}
	if bufClus != nil {
		if err := bufClus.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func parseFlags() *options {
	o := &options{}
	flag.StringVar(&o.inputPath, "i", "", "Input file (default: stdin)")
	flag.StringVar(&o.inputPath, "input", "", "Input file (default: stdin)")
	flag.StringVar(&o.outputPath, "o", "", "Output file (default: stdout)")
	flag.StringVar(&o.outputPath, "output", "", "Output file (default: stdout)")
	mode := string(modeHybrid)
	flag.StringVar(&mode, "m", mode, "Similarity mode: exact|canonical|struct|simhash|hybrid (default: hybrid)")
	flag.StringVar(&mode, "mode", mode, "Similarity mode: exact|canonical|struct|simhash|hybrid (default: hybrid)")
	flag.Float64Var(&o.threshold, "t", defaultThresholdSimhash, "Similarity threshold (mode-dependent)")
	flag.Float64Var(&o.threshold, "threshold", defaultThresholdSimhash, "Similarity threshold (mode-dependent)")
	norm := string(normStrict)
	flag.StringVar(&norm, "n", norm, "Normalization: none|loose|strict (default: strict)")
	flag.StringVar(&norm, "normalize", norm, "Normalization: none|loose|strict (default: strict)")
	keep := string(keepRichest)
	flag.StringVar(&keep, "k", keep, "Which URL to keep per cluster: first|shortest|longest|richest|lexi (default: richest)")
	flag.StringVar(&keep, "keep", keep, "Which URL to keep per cluster: first|shortest|longest|richest|lexi (default: richest)")
	ds := string(scopeHost)
	flag.StringVar(&ds, "d", ds, "Domain scope: all|registrable|host (default: host)")
	flag.StringVar(&ds, "domain-scope", ds, "Domain scope: all|registrable|host (default: host)")
	flag.IntVar(&o.parallel, "p", runtime.NumCPU(), "Worker goroutines (default: number of CPUs)")
	flag.IntVar(&o.parallel, "parallel", runtime.NumCPU(), "Worker goroutines (default: number of CPUs)")
	flag.IntVar(&o.bufferBytes, "B", 1<<20, "Line buffer size (default: 1<<20)")
	flag.IntVar(&o.bufferBytes, "buffer-bytes", 1<<20, "Line buffer size (default: 1<<20)")
	ex := ""
	flag.StringVar(&ex, "x", ex, "Comma-separated params to drop (e.g., utm_*, gclid, fbclid)")
	flag.StringVar(&ex, "exclude-params", ex, "Comma-separated params to drop (e.g., utm_*, gclid, fbclid)")
	only := ""
	flag.StringVar(&only, "X", only, "Only keep these params (whitelist); overrides exclude")
	flag.StringVar(&only, "only-params", only, "Only keep these params (whitelist); overrides exclude")
	flag.BoolVar(&o.quiet, "q", false, "Suppress stats to stderr")
	flag.BoolVar(&o.quiet, "quiet", false, "Suppress stats to stderr")
	flag.StringVar(&o.clustersPath, "C", "", "Also write clusters (rep + members) to FILE (newline-separated blocks)")
	flag.StringVar(&o.clustersPath, "clusters", "", "Also write clusters (rep + members) to FILE (newline-separated blocks)")
	flag.StringVar(&o.seedPath, "S", "", "Preload known URLs to bias cluster representatives")
	flag.StringVar(&o.seedPath, "seed", "", "Preload known URLs to bias cluster representatives")
	flag.IntVar(&o.simhashBits, "simhash-bits", 64, "SimHash bits: 64 or 128 (default: 64)")
	flag.IntVar(&o.simhashShingle, "simhash-shingle", 3, "Token shingle size (default: 3)")
	flag.Float64Var(&o.structWeight, "struct-weight", 0.6, "Weight of structural vs query similarity in hybrid (default: 0.6)")
	flag.BoolVar(&o.noIDNA, "no-idna", false, "Do not punycode/IDNA-normalize hostnames")
	flag.BoolVar(&o.httpEqHttps, "http-eq-https", false, "Treat http and https as equivalent for similarity scoring")
	flag.BoolVar(&o.version, "version", false, "Print version and exit")
	flag.BoolVar(&o.verbose, "v", false, "Enable verbose progress logging")
	flag.BoolVar(&o.verbose, "verbose", false, "Enable verbose progress logging")
	flag.IntVar(&o.progressEveryS, "progress-interval", 1, "Progress update interval in seconds (with -v)")
	drops := ""
	flag.StringVar(&drops, "drop-ext", drops, "Comma-separated file extensions to drop when they are the path suffix (e.g., gif,jpg,png)")
	flag.BoolVar(&o.dropB64ish, "drop-b64ish", false, "Drop URLs whose path contains base64-ish long tokens")
	flag.BoolVar(&o.dropGibberish, "drop-gibberish", false, "Drop URLs whose path contains long alnum+ tokens (likely gibberish)")
	// Preset for quick use with recommended defaults (generic)
	flag.BoolVar(&o.presetClean, "preset-clean", false, "Apply recommended defaults for large recon lists (alias for a set of flags)")
	flag.BoolVar(&o.presetClean, "preset", false, "Alias of --preset-clean (recommended defaults)")
	// Convenience flag to cluster within registrable (eTLD+1) instead of host
	flag.BoolVar(&o.registrableFlag, "registrable-scope", false, "Cluster within registrable domain (eTLD+1); overrides default host scope unless -d is provided")
	flag.Parse()

	// Track which flags user actually set
	visited := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	provided := func(names ...string) bool {
		for _, n := range names {
			if visited[n] {
				return true
			}
		}
		return false
	}

	o.mode = modeType(strings.ToLower(mode))
	o.normalize = normalizeLevel(strings.ToLower(norm))
	o.keep = keepPolicy(strings.ToLower(keep))
	o.domainScope = domainScope(strings.ToLower(ds))

	// Apply registrable convenience flag only if user didn't explicitly set -d/--domain-scope
	if o.registrableFlag && !provided("d", "domain-scope") {
		o.domainScope = scopeRegistrable
	}

	if o.mode == modeStruct && (o.threshold == 0 || o.threshold == defaultThresholdSimhash) {
		// default jaccard threshold
		o.threshold = defaultThresholdStruct
	}
	if o.simhashBits != 64 && o.simhashBits != 128 {
		o.simhashBits = 64
	}
	if ex != "" {
		o.excludeParams = splitCSV(ex)
	}
	if only != "" {
		o.onlyParams = splitCSV(only)
	}
	if drops != "" {
		o.dropExts = splitCSV(drops)
	}
	if o.parallel < 1 {
		o.parallel = 1
	}

	// Apply preset defaults (respect user-provided flags; only set when not explicitly provided)
	if o.presetClean {
		if !provided("m", "mode") {
			o.mode = modeHybrid
		}
		if !provided("n", "normalize") {
			o.normalize = normStrict
		}
		// Do not change domain scope in preset; rely on defaults or user flags
		if !provided("x", "exclude-params") && len(o.excludeParams) == 0 {
			o.excludeParams = []string{"utm_*", "gclid", "fbclid", "_ga", "_gid", "ref", "sid", "session", "phpsessid", "JSESSIONID"}
		}
		if !provided("X", "only-params") {
			// leave user's only-params if provided; otherwise ensure nil so exclude applies
			// no action needed if user set it
		}
		if !provided("http-eq-https") {
			o.httpEqHttps = true
		}
		if !provided("p", "parallel") {
			o.parallel = runtime.NumCPU()
		}
		if !provided("B", "buffer-bytes") {
			o.bufferBytes = 1 << 22
		}
		if !provided("drop-ext") && len(o.dropExts) == 0 {
			o.dropExts = []string{"gif"}
		}
		if !provided("drop-b64ish") {
			o.dropB64ish = true
		}
		if !provided("drop-gibberish") {
			o.dropGibberish = true
		}
		if !provided("v", "verbose") {
			o.verbose = true
		}
		if !provided("progress-interval") {
			if o.progressEveryS <= 0 {
				o.progressEveryS = 1
			}
		}
	}
	return o
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func openOptional(path string, w bool) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	if w {
		return os.Create(path)
	}
	return os.Open(path)
}

func main() {
	o := parseFlags()
	if o.version {
		fmt.Println("reuniq v0.1.0")
		return
	}

	if !o.quiet {
		fmt.Fprint(os.Stderr, banner)
	}

	// If a positional file is provided, prefer it for input unless -i specified
	if o.inputPath == "" {
		args := flag.Args()
		if len(args) > 0 {
			o.inputPath = args[0]
		}
	}

	var in io.Reader = os.Stdin
	var out io.Writer = os.Stdout
	var clusOut io.Writer
	var inFile, outFile, clusFile *os.File
	var err error

	if o.inputPath != "" {
		inFile, err = openOptional(o.inputPath, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error opening input:", err)
			os.Exit(1)
		}
		defer inFile.Close()
		in = inFile
	}
	if o.outputPath != "" {
		outFile, err = openOptional(o.outputPath, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error opening output:", err)
			os.Exit(1)
		}
		defer outFile.Close()
		out = outFile
	}
	if o.clustersPath != "" {
		clusFile, err = openOptional(o.clustersPath, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error opening clusters file:", err)
			os.Exit(1)
		}
		defer clusFile.Close()
		clusOut = clusFile
	}

	start := time.Now()
	st, err := process(o, in, out, clusOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if !o.quiet {
		elapsed := time.Since(start)
		fmt.Fprintf(os.Stderr, "lines=%d parsed=%d skipped=%d clusters=%d merged=%d time=%s\n",
			atomic.LoadInt64(&st.linesRead), atomic.LoadInt64(&st.linesParsed), atomic.LoadInt64(&st.linesSkipped), atomic.LoadInt64(&st.clusters), atomic.LoadInt64(&st.merged), elapsed)
	}
}
