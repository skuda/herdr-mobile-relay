package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

const cacheTTL = 60 * time.Second

type cacheEntry struct {
	name     string
	location conversation.Location
	expires  time.Time
}

type Resolver struct {
	mu         sync.Mutex
	cache      map[string]cacheEntry
	titleScans map[string]*titleScan
	home       string
	reader     *conversation.Reader
}

// NewResolver creates an independent title and transcript resolver.
func NewResolver(home string) *Resolver {
	return NewResolverWithReader(home, conversation.NewReader(home))
}

// NewResolverWithReader shares transcript-location decisions with conversation
// history consumers.
func NewResolverWithReader(home string, reader *conversation.Reader) *Resolver {
	if reader == nil {
		reader = conversation.NewReader(home)
	}
	return &Resolver{cache: make(map[string]cacheEntry), home: home, reader: reader}
}

// Root accessors remain useful in tests that pin configuration precedence.
func (r *Resolver) claudeRoots() []string { return agentroots.Claude(r.home) }
func (r *Resolver) qoderRoots() []string  { return agentroots.Qoder(r.home) }
func (r *Resolver) codexHomes() []string  { return agentroots.CodexHomes(r.home) }
func (r *Resolver) piRoots() []string     { return agentroots.Pi(r.home) }
func (r *Resolver) ompRoots() []string    { return agentroots.OMP(r.home) }

func (r *Resolver) SessionName(agent, cwd, sessionID string) string {
	return r.SessionNameWithProject(agent, conversation.ProjectContext{CWD: cwd}, sessionID)
}

// SessionNameWithProject resolves a title through the same directory-aware
// location as conversation history. The normalized project context is part of
// the title cache identity, so a foreground-only change cannot reuse a title
// from another project copy.
func (r *Resolver) SessionNameWithProject(agent string, project conversation.ProjectContext, sessionID string) string {
	project = conversation.NormalizeProjectContext(agent, project)
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	agentLower := strings.ToLower(strings.TrimSpace(agent))
	key := sessionCacheKey(agentLower, project, sessionID)
	location := r.reader.LocateWithProject(agent, project, sessionID)
	if location.Path == "" {
		return ""
	}
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && entry.location == location && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.name
	}
	r.mu.Unlock()

	var name string
	switch {
	case isOMPSessionAgent(agentLower):
		name = extractOMPSessionTitle(location.Path)
	case isPiSessionAgent(agentLower):
		name = extractPiSessionTitle(location.Path)
	case isHermesSessionAgent(agentLower), conversation.IsKimi(agentLower):
		name = location.Title
	case strings.Contains(agentLower, "qoder"), strings.Contains(agentLower, "claude"):
		name = r.extractTitle(location.Path)
	case strings.Contains(agentLower, "codex"):
		name = codexIndexThreadName(filepath.Join(filepath.Dir(location.Root), "session_index.jsonl"), sessionID)
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{name: name, location: location, expires: now.Add(cacheTTL)}
	r.mu.Unlock()
	return name
}

func sessionCacheKey(agent string, project conversation.ProjectContext, sessionID string) string {
	if project.ForegroundCWD == "" {
		// Preserve the pane-only key for existing callers and cache tests.
		return agent + "|" + project.CWD + "|" + sessionID
	}
	return agent + "|" + project.CWD + "|" + project.ForegroundCWD + "|" + sessionID
}

func isOMPSessionAgent(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "omp", "oh-my-pi", "oh my pi", "ohmypi":
		return true
	default:
		return false
	}
}

func isPiSessionAgent(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "pi", "pi-coding-agent":
		return true
	default:
		return false
	}
}

func isHermesSessionAgent(agent string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(agent)))
	return normalized == "hermes" || normalized == "hermesagent"
}

func extractOMPSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var sessionTitle string
	var headerTitle string
	hasHeaderTitle := false
	var latestTitle string
	hasTitleEvent := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		switch record.Type {
		case "title":
			if !hasHeaderTitle {
				hasHeaderTitle = true
				headerTitle = strings.TrimSpace(record.Title)
			}
		case "session":
			sessionTitle = strings.TrimSpace(record.Title)
		case "title_change":
			hasTitleEvent = true
			latestTitle = strings.TrimSpace(record.Title)
		}
	}
	if hasHeaderTitle {
		return headerTitle
	}
	if hasTitleEvent {
		return latestTitle
	}
	return sessionTitle
}

func extractPiSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var name string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.Type == "session_info" {
			name = strings.TrimSpace(record.Name)
		}
	}
	return name
}

func codexIndexThreadName(indexFile, sessionID string) string {
	f, err := os.Open(indexFile)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.ID == sessionID {
			return record.ThreadName
		}
	}
	return ""
}

var titleFields = []string{"customTitle", "aiTitle", "title", "summary", "text", "name", "value"}
var titleTypes = map[string]bool{"custom-title": true, "ai-title": true, "summary": true}

// titleTypeTokens are the quoted type values of the title records. A line
// without one of them is not decoded.
var titleTypeTokens = [][]byte{[]byte(`"custom-title"`), []byte(`"ai-title"`), []byte(`"summary"`)}

const (
	maxTitleLineBytes  = 1024 * 1024
	titleFingerprint   = 4096
	maxTitleScanStates = 1024
)

// titleScan remembers how far a transcript was read and the titles found so
// far, so a refresh starts at the offset where the previous one stopped.
type titleScan struct {
	mu     sync.Mutex
	info   os.FileInfo
	offset int64
	head   []byte
	tail   []byte
	found  map[string]string
}

func (r *Resolver) extractTitle(path string) string {
	r.mu.Lock()
	if r.titleScans == nil {
		r.titleScans = make(map[string]*titleScan)
	}
	scan := r.titleScans[path]
	if scan == nil {
		if len(r.titleScans) >= maxTitleScanStates {
			clear(r.titleScans)
		}
		scan = &titleScan{}
		r.titleScans[path] = scan
	}
	r.mu.Unlock()
	return scan.title(path)
}

func (s *titleScan) title(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		s.reset(nil)
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.reset(nil)
		return ""
	}
	if !s.continues(f, info) {
		s.reset(info)
	}
	s.info = info
	if info.Size() > s.offset {
		s.readFrom(f)
	}
	for _, recordType := range []string{"custom-title", "ai-title", "summary"} {
		if s.found[recordType] != "" {
			return s.found[recordType]
		}
	}
	return ""
}

func (s *titleScan) reset(info os.FileInfo) {
	s.info = info
	s.offset = 0
	s.head = nil
	s.tail = nil
	s.found = make(map[string]string)
}

// continues reports whether the scan can resume at its offset: the same file
// identity, no shrink, and unchanged bytes at the start and just before the
// offset.
func (s *titleScan) continues(f *os.File, info os.FileInfo) bool {
	if s.info == nil || s.found == nil || !os.SameFile(s.info, info) || info.Size() < s.offset {
		return false
	}
	return sameBytesAt(f, 0, s.head) && sameBytesAt(f, s.offset-int64(len(s.tail)), s.tail)
}

func sameBytesAt(f *os.File, offset int64, want []byte) bool {
	if len(want) == 0 {
		return true
	}
	got := make([]byte, len(want))
	n, err := f.ReadAt(got, offset)
	return n == len(want) && (err == nil || errors.Is(err, io.EOF)) && bytes.Equal(got, want)
}

// readFrom decodes the lines after the scanned offset and advances the offset
// past each line that ends in a newline. A final line without its newline is
// decoded but not consumed, so the next refresh reads it again. A line longer
// than maxTitleLineBytes is skipped.
func (s *titleScan) readFrom(f *os.File) {
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return
	}
	reader := bufio.NewReaderSize(f, 256*1024)
	var line []byte
	var lineBytes int64
	for {
		chunk, err := reader.ReadSlice('\n')
		lineBytes += int64(len(chunk))
		if lineBytes <= maxTitleLineBytes {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if lineBytes <= maxTitleLineBytes && len(line) > 0 {
			s.record(line)
		}
		if err != nil {
			break
		}
		s.offset += lineBytes
		line = line[:0]
		lineBytes = 0
	}
	s.remember(f)
}

func (s *titleScan) record(line []byte) {
	matched := false
	for _, token := range titleTypeTokens {
		if bytes.Contains(line, token) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	var record map[string]any
	if json.Unmarshal(line, &record) != nil {
		return
	}
	recordType, _ := record["type"].(string)
	if !titleTypes[recordType] {
		return
	}
	for _, field := range titleFields {
		if value, ok := record[field].(string); ok && strings.TrimSpace(value) != "" {
			s.found[recordType] = strings.TrimSpace(value)
			return
		}
	}
}

func (s *titleScan) remember(f *os.File) {
	s.head = readBytesAt(f, 0, min(s.offset, titleFingerprint))
	tailLength := min(s.offset, titleFingerprint)
	s.tail = readBytesAt(f, s.offset-tailLength, tailLength)
}

func readBytesAt(f *os.File, offset, length int64) []byte {
	if length <= 0 {
		return nil
	}
	data := make([]byte, length)
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return data[:n]
}
