package collectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

type usageFile struct {
	path          string
	parentSession string
}

func findUsageFiles(root string) ([]usageFile, error) {
	return buddyFilesFromRoot(root)
}

func cursorKey(sourceID, path string) string {
	return sourceID + ":" + path
}

func cursorMap(cursors []Cursor) map[string]Cursor {
	indexed := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID != "" && cursor.FilePath != "" {
			indexed[cursorKey(cursor.SourceID, cursor.FilePath)] = cursor
		}
	}
	return indexed
}

// incrementalOffset resumes only append-only files. Shorter files were rewritten;
// stable event IDs make replaying them safe.
func incrementalOffset(cursor Cursor, hasCursor bool, size int64) int64 {
	if !hasCursor || cursor.Offset < 0 || cursor.Offset > size {
		return 0
	}
	return cursor.Offset
}

func readJSONLines(path string, offset int64) (events []map[string]any, next int64, read int64, damaged int, incomplete bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, 0, 0, false, err
	}
	defer file.Close()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, 0, 0, false, err
	}

	reader := bufio.NewReader(file)
	next = offset
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return events, next, read, damaged, false, readErr
		}
		if len(line) == 0 {
			break
		}
		if readErr == io.EOF || len(line) == 0 || line[len(line)-1] != '\n' {
			// A live writer may have flushed only part of the final record.
			incomplete = true
			break
		}
		next += int64(len(line))
		read += int64(len(line))
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var record map[string]any
		if decodeErr := decoder.Decode(&record); decodeErr != nil {
			damaged++
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			damaged++
			continue
		}
		events = append(events, record)
	}
	return events, next, read, damaged, incomplete, nil
}

func buddyFilesFromRoot(root string) ([]usageFile, error) {
	projects := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projects)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var files []usageFile
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(projects, project.Name())
		sessionEntries, err := os.ReadDir(projectDir)
		if err != nil {
			return nil, err
		}
		for _, session := range sessionEntries {
			path := filepath.Join(projectDir, session.Name())
			if session.IsDir() {
				subagentEntries, err := os.ReadDir(filepath.Join(path, "subagents"))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				for _, subagent := range subagentEntries {
					subagentPath := filepath.Join(path, "subagents", subagent.Name())
					if !subagent.IsDir() && strings.EqualFold(filepath.Ext(subagent.Name()), ".jsonl") {
						files = append(files, usageFile{path: subagentPath, parentSession: session.Name()})
					}
				}
				continue
			}
			if strings.EqualFold(filepath.Ext(session.Name()), ".jsonl") {
				files = append(files, usageFile{path: path})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func asObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func asArray(value any) []any {
	array, _ := value.([]any)
	return array
}

func text(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func uintValue(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
			if parsed >= 0 {
				return uint64(parsed)
			}
			return 0
		}
		if parsed, err := strconv.ParseUint(typed.String(), 10, 64); err == nil {
			return parsed
		}
		if parsed, err := strconv.ParseFloat(typed.String(), 64); err == nil && parsed >= 0 {
			return uint64(parsed)
		}
	case float64:
		if typed >= 0 {
			return uint64(typed)
		}
	case float32:
		if typed >= 0 {
			return uint64(typed)
		}
	case int:
		if typed >= 0 {
			return uint64(typed)
		}
	case int8:
		if typed >= 0 {
			return uint64(typed)
		}
	case int16:
		if typed >= 0 {
			return uint64(typed)
		}
	case int32:
		if typed >= 0 {
			return uint64(typed)
		}
	case int64:
		if typed >= 0 {
			return uint64(typed)
		}
	case uint:
		return uint64(typed)
	case uint8:
		return uint64(typed)
	case uint16:
		return uint64(typed)
	case uint32:
		return uint64(typed)
	case uint64:
		return typed
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func identityText(value any) string {
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	return text(value)
}

func nested(object map[string]any, keys ...string) any {
	var current any = object
	for _, key := range keys {
		parent, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = parent[key]
	}
	return current
}

func objectText(object map[string]any, key string) string {
	return strings.TrimSpace(text(object[key]))
}

func objectHas(object map[string]any, key string) bool {
	_, ok := object[key]
	return ok
}

func saturatingSub(total uint64, parts ...uint64) uint64 {
	for _, part := range parts {
		if part >= total {
			return 0
		}
		total -= part
	}
	return total
}

func saturatingAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func maxUint(values ...uint64) uint64 {
	var result uint64
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func timestampMillis(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case json.Number:
		if milliseconds, err := typed.Int64(); err == nil {
			return time.UnixMilli(milliseconds).UTC(), true
		}
		if seconds, err := strconv.ParseFloat(typed.String(), 64); err == nil {
			if seconds >= 1e12 {
				return time.UnixMilli(int64(seconds)).UTC(), true
			}
			return time.UnixMilli(int64(seconds * 1000)).UTC(), true
		}
	case float64:
		if typed >= 1e12 {
			return time.UnixMilli(int64(typed)).UTC(), true
		}
		return time.UnixMilli(int64(typed * 1000)).UTC(), true
	case int64:
		return time.UnixMilli(typed).UTC(), true
	case int:
		if int64(typed) >= 1e12 {
			return time.UnixMilli(int64(typed)).UTC(), true
		}
		return time.UnixMilli(int64(typed) * 1000).UTC(), true
	case string:
		raw := strings.TrimSpace(typed)
		if raw != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				return parsed.UTC(), true
			}
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				return timestampMillis(parsed)
			}
		}
	}
	return time.Time{}, false
}

func sessionEvent(deviceID, app, sourceRecordID, requestID, sessionID, modelName string, usage model.TokenUsage, occurredAt time.Time) model.UsageEvent {
	usage.Model = modelName
	event := model.NewUsageEvent(deviceID, model.SourceSession, app, sourceRecordID, modelName, usage, occurredAt)
	event.RequestID = requestID
	event.SessionID = sessionID
	event.RequestModel = modelName
	event.IsStreaming = true
	event.InputTokenSemantics = model.SemanticsFresh
	return event
}
