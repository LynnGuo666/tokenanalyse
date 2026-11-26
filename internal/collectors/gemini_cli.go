package collectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

const (
	geminiCLISourceID  = "gemini-cli"
	geminiCLISourceApp = "gemini"
)

// GeminiCliCollector reads Gemini CLI's legacy JSON conversations and the
// newer JSONL replay format. Token values are per message, not cumulative.
type GeminiCliCollector struct{}

var _ Collector = (*GeminiCliCollector)(nil)

func NewGeminiCliCollector() *GeminiCliCollector { return &GeminiCliCollector{} }

func (c *GeminiCliCollector) Info() Info {
	return Info{
		ID:          geminiCLISourceID,
		Name:        "Gemini CLI",
		AppTypes:    []string{geminiCLISourceApp},
		Description: "Read token usage from Gemini CLI JSON and JSONL chats",
		Incremental: true,
	}
}

type geminiCLIFileState struct {
	Messages map[string]string `json:"messages"`
}

func (c *GeminiCliCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{}
	if strings.TrimSpace(ctx.Paths.Gemini) == "" {
		return result, nil
	}

	cursorByPath := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID == geminiCLISourceID {
			cursorByPath[cursor.FilePath] = cursor
		}
	}

	for _, path := range geminiCLISessionFiles(ctx.Paths.Gemini) {
		result.FilesScanned++
		partial, err := c.collectFile(ctx, path, cursorByPath[path])
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Gemini CLI session %s: %v", path, err))
			continue
		}
		result.Merge(partial)
	}
	return result, nil
}

func geminiCLISessionFiles(geminiHome string) []string {
	projects, err := os.ReadDir(filepath.Join(geminiHome, "tmp"))
	if err != nil {
		return nil
	}

	var files []string
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		chatsDir := filepath.Join(geminiHome, "tmp", project.Name(), "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "session-") {
				continue
			}
			path := filepath.Join(chatsDir, name)
			switch filepath.Ext(name) {
			case ".jsonl":
				files = append(files, path)
			case ".json":
				// A resumed conversation retains the old JSON file. The JSONL
				// replay is authoritative, so reading both would duplicate usage.
				if _, err := os.Stat(strings.TrimSuffix(path, ".json") + ".jsonl"); os.IsNotExist(err) {
					files = append(files, path)
				}
			}
		}
	}
	sort.Strings(files)
	return files
}

func (c *GeminiCliCollector) collectFile(ctx Context, path string, existing Cursor) (Result, error) {
	result := Result{}
	info, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("not a regular file")
	}
	modified := info.ModTime().UnixNano()
	size := info.Size()
	if existing.FilePath != "" && modified <= existing.ModifiedNanos && size <= existing.Offset {
		result.Cursors = append(result.Cursors, existing)
		return result, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	result.BytesRead = int64(len(data))
	document, ok := geminiCLIParseSessionDocument(data)
	if !ok {
		return result, fmt.Errorf("session document is not valid JSON or JSONL")
	}

	state := geminiCLIFileState{Messages: map[string]string{}}
	if existing.State != "" {
		// A corrupt state must not make an otherwise readable session unusable.
		_ = json.Unmarshal([]byte(existing.State), &state)
		if state.Messages == nil {
			state.Messages = map[string]string{}
		}
	}

	sessionID := geminiCLIString(document["sessionId"])
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	fallbackTime := info.ModTime().UTC()
	messages, _ := document["messages"].([]any)
	for index, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || geminiCLIString(message["type"]) != "gemini" {
			continue
		}
		tokens, ok := message["tokens"].(map[string]any)
		if !ok {
			continue
		}
		input := geminiCLIUint64(tokens["input"])
		output := geminiCLIUint64(tokens["output"])
		cached := geminiCLIUint64(tokens["cached"])
		thoughts := geminiCLIUint64(tokens["thoughts"])
		if input == 0 && output == 0 && cached == 0 && thoughts == 0 {
			continue
		}

		messageID := geminiCLIString(message["id"])
		if messageID == "" {
			messageID = fmt.Sprintf("index-%d", index)
		}
		modelName := geminiCLIString(message["model"])
		if modelName == "" {
			modelName = "unknown"
		}
		occurredAt := geminiCLITimestamp(message["timestamp"], fallbackTime)
		fingerprint := geminiCLIFingerprint(modelName, input, output, cached, thoughts, occurredAt)
		if state.Messages[messageID] == fingerprint {
			continue
		}

		recordID := sessionID + ":" + messageID
		usage := model.TokenUsage{
			InputTokens:           input,
			OutputTokens:          geminiCLIAdd(output, thoughts),
			CacheReadTokens:       cached,
			CacheCreationTokens:   0,
			CacheCreation5MTokens: 0,
			CacheCreation1HTokens: 0,
			Model:                 modelName,
			MessageID:             messageID,
		}
		event := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, geminiCLISourceApp, recordID, modelName, usage, occurredAt)
		event.RequestID = "gemini_session:" + recordID
		event.SessionID = sessionID
		event.ProviderID = "_gemini_session"
		event.ProviderName = "Gemini CLI"
		event.RequestModel = modelName
		event.PricingModel = modelName
		event.InputTokenSemantics = model.SemanticsTotal
		event.TimingQuality = model.TimingUnknown
		event.IsStreaming = true
		statusCode := 200
		event.StatusCode = &statusCode
		event.Metadata["dataSource"] = "gemini_session"
		event.Metadata["providerType"] = "gemini_session"
		event.Metadata["thoughtsTokens"] = thoughts
		result.Events = append(result.Events, event)
		state.Messages[messageID] = fingerprint
	}

	encodedState, err := json.Marshal(state)
	if err != nil {
		return Result{}, fmt.Errorf("encode cursor state: %w", err)
	}
	result.Cursors = append(result.Cursors, Cursor{
		SourceID:      geminiCLISourceID,
		FilePath:      path,
		Offset:        size,
		ModifiedNanos: modified,
		State:         string(encodedState),
		UpdatedAt:     time.Now().UTC(),
	})
	return result, nil
}

// geminiCLIParseSessionDocument restores the legacy {sessionId, messages}
// shape. JSONL follows Gemini's loadConversationRecord replay rules: message
// upserts, $set checkpoints, and $rewindTo markers.
func geminiCLIParseSessionDocument(data []byte) (map[string]any, bool) {
	var whole any
	if err := json.Unmarshal(data, &whole); err == nil {
		if object, ok := whole.(map[string]any); ok {
			if _, exists := object["messages"]; !exists {
				object["messages"] = []any{}
			}
			return object, true
		}
		return nil, false
	}

	metadata := map[string]any{}
	messages := []any{}
	messageIndexes := map[string]int{}
	upsert := func(message any) {
		object, ok := message.(map[string]any)
		if !ok {
			return
		}
		id := geminiCLIString(object["id"])
		if id == "" {
			return
		}
		if index, exists := messageIndexes[id]; exists {
			messages[index] = object
			return
		}
		messageIndexes[id] = len(messages)
		messages = append(messages, object)
	}
	reset := func(list []any) {
		messages = nil
		messageIndexes = map[string]int{}
		for _, message := range list {
			upsert(message)
		}
	}

	sawRecord := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil || record == nil {
			continue
		}
		sawRecord = true
		if rewindTo, ok := record["$rewindTo"].(string); ok {
			length := 0
			if index, exists := messageIndexes[rewindTo]; exists {
				length = index
			}
			for id, index := range messageIndexes {
				if index >= length {
					delete(messageIndexes, id)
				}
			}
			messages = messages[:length]
			continue
		}
		if _, ok := record["id"].(string); ok {
			upsert(record)
			continue
		}
		if setValue, exists := record["$set"]; exists {
			if set, ok := setValue.(map[string]any); ok {
				if list, ok := set["messages"].([]any); ok {
					reset(list)
					delete(set, "messages")
				}
				for key, value := range set {
					metadata[key] = value
				}
			}
			continue
		}
		if _, ok := record["sessionId"].(string); ok {
			if list, ok := record["messages"].([]any); ok {
				for _, message := range list {
					upsert(message)
				}
			}
			for key, value := range record {
				metadata[key] = value
			}
		}
	}
	if !sawRecord {
		return nil, false
	}
	metadata["messages"] = messages
	return metadata, true
}

func geminiCLIFingerprint(modelName string, input, output, cached, thoughts uint64, occurredAt time.Time) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\x00%d\x00%d", modelName, input, output, cached, thoughts, occurredAt.UnixNano())
	return hex.EncodeToString(hash.Sum(nil))
}

func geminiCLIString(value any) string {
	text, _ := value.(string)
	return text
}

func geminiCLIUint64(value any) uint64 {
	switch number := value.(type) {
	case float64:
		if number >= 0 {
			return uint64(number)
		}
	case json.Number:
		parsed, err := number.Int64()
		if err == nil && parsed >= 0 {
			return uint64(parsed)
		}
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(number), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func geminiCLITimestamp(value any, fallback time.Time) time.Time {
	text, ok := value.(string)
	if !ok {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return fallback
	}
	return parsed.UTC()
}

func geminiCLIAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}
