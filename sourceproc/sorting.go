package sourceproc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"m3u-stream-merger/config"
	"m3u-stream-merger/logger"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cespare/xxhash"
)

const mutexShards = 4096

type SortingManager struct {
	muxes      []*sync.Mutex       // Sharded mutexes
	caches     []map[string]string // Sharded caches mapping sanitized titles to filenames
	sortingKey string
	sortingDir string
}

func newSortingManager() *SortingManager {
	sortingKey := os.Getenv("SORTING_KEY")
	sortingDir := strings.ToLower(os.Getenv("SORTING_DIRECTION"))
	basePath := config.GetSortDirPath()
	err := os.MkdirAll(basePath, 0755) // Create basePath once here
	if err != nil {
		logger.Default.Error(err.Error())
	}

	muxes := make([]*sync.Mutex, mutexShards)
	caches := make([]map[string]string, mutexShards)
	for i := range muxes {
		muxes[i] = &sync.Mutex{}
		caches[i] = make(map[string]string)
	}

	return &SortingManager{
		muxes:      muxes,
		caches:     caches,
		sortingKey: sortingKey,
		sortingDir: sortingDir,
	}
}

func (m *SortingManager) AddToSorter(s *StreamInfo) error {
	basePath := config.GetSortDirPath()

	var primaryField string
	switch m.sortingKey {
	case "tvg-id":
		primaryField = normalizeNumericField(s.TvgID, 10, m.sortingDir)
	case "tvg-chno", "channel-id", "channel-number":
		primaryField = normalizeNumericField(s.TvgChNo, 10, m.sortingDir)
	case "tvg-group", "group-title":
		primaryField = normalizeStringField(s.Group, m.sortingDir)
	case "tvg-type":
		primaryField = normalizeStringField(s.TvgType, m.sortingDir)
	case "source":
		primaryField = normalizeNumericField(s.SourceM3U, 5, m.sortingDir)
	default: // Default to sorting by title
		primaryField = normalizeStringField(s.Title, m.sortingDir)
	}

	sourceM3U := normalizeNumericField(s.SourceM3U, 5, "asc") // Always ascending
	sourceIndex := fmt.Sprintf("%05d", s.SourceIndex)         // Always ascending

	group := sanitizeField(s.Group)
	tvgType := sanitizeField(s.TvgType)
	title := sanitizeField(s.Title)
	sanitizedTitle := sanitizeField(s.Title)

	filename := fmt.Sprintf("%s_%s_%s_%s_%s_%s_%s.json",
		primaryField, group, tvgType, sourceM3U, sourceIndex, title, m.sortingKey)

	fullPath := filepath.Join(basePath, filename)

	hash := xxhash.Sum64String(s.Title)
	shardIndex := hash % mutexShards
	mutex := m.muxes[shardIndex]
	mutex.Lock()
	defer mutex.Unlock()

	// Check cache first
	if existingFile, exists := m.caches[shardIndex][sanitizedTitle]; exists {
		if err := mergeStreamInfo(existingFile, s); err != nil {
			return fmt.Errorf("failed to merge StreamInfo: %w", err)
		}
		return nil
	}

	// Create new file and update cache
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("file already exists: %s", fullPath)
		}
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	if err := writeStreamInfoToFile(file, s); err != nil {
		return fmt.Errorf("failed to write StreamInfo to file: %w", err)
	}

	m.caches[shardIndex][sanitizedTitle] = fullPath
	return nil
}

func (m *SortingManager) Close() {
	basePath := config.GetSortDirPath()
	os.RemoveAll(basePath)
}

func (m *SortingManager) GetSortedEntries(callback func(*StreamInfo)) error {
	basePath := config.GetSortDirPath()

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return fmt.Errorf("sorting directory does not exist: %s", basePath)
	}

	entries, err := os.ReadDir(basePath)
	if err != nil {
		return fmt.Errorf("failed to read sorting directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(basePath, entry.Name())
		streamInfo, err := readStreamInfoFromFile(filePath)
		if err != nil {
			logger.Default.Errorf("Failed to read StreamInfo from file %s: %v", filePath, err)
			continue
		}

		// Pass the entry to the callback
		callback(streamInfo)
	}

	return nil
}

func mergeStreamInfo(existingFile string, newStream *StreamInfo) error {
	existingStream, err := readStreamInfoFromFile(existingFile)
	if err != nil {
		return fmt.Errorf("failed to read existing StreamInfo: %w", err)
	}

	mergedStream := mergeStreamInfoAttributes(existingStream, newStream)

	file, err := os.OpenFile(existingFile, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open existing file for writing: %w", err)
	}
	defer file.Close()

	if err := writeStreamInfoToFile(file, mergedStream); err != nil {
		return fmt.Errorf("failed to write merged StreamInfo to file: %w", err)
	}

	return nil
}

func mergeStreamInfoAttributes(base, new *StreamInfo) *StreamInfo {
	if base.Title == "" {
		base.Title = new.Title
	}
	if base.TvgID == "" {
		base.TvgID = new.TvgID
	}
	if base.TvgChNo == "" {
		base.TvgChNo = new.TvgChNo
	}
	if base.TvgType == "" {
		base.TvgType = new.TvgType
	}
	if base.LogoURL == "" {
		base.LogoURL = new.LogoURL
	}
	if base.Group == "" {
		base.Group = new.Group
	}

	if base.URLs == nil {
		base.URLs = make(map[string]map[string]string)
	}
	for key, value := range new.URLs {
		if _, exists := base.URLs[key]; !exists {
			base.URLs[key] = value
		} else {
			for subKey, subValue := range value {
				base.URLs[key][subKey] = subValue
			}
		}
	}

	if new.SourceM3U < base.SourceM3U || (new.SourceM3U == base.SourceM3U && new.SourceIndex < base.SourceIndex) {
		base.SourceM3U = new.SourceM3U
		base.SourceIndex = new.SourceIndex
	}

	return base
}

func readStreamInfoFromFile(filename string) (*StreamInfo, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var stream StreamInfo
	if err := json.NewDecoder(file).Decode(&stream); err != nil {
		return nil, fmt.Errorf("failed to decode StreamInfo: %w", err)
	}

	return &stream, nil
}

func writeStreamInfoToFile(file *os.File, stream *StreamInfo) error {
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(stream); err != nil {
		return err
	}
	return writer.Flush()
}

func normalizeNumericField(value string, width int, direction string) string {
	num, err := strconv.Atoi(value)
	if err != nil {
		return sanitizeField(value)
	}
	if direction == "desc" {
		maxValue := int64(1<<31 - 1) // Use a large constant (e.g., max int32)
		return fmt.Sprintf("%0*d", width, maxValue-int64(num))
	}
	return fmt.Sprintf("%0*d", width, num)
}

func normalizeStringField(value, direction string) string {
	if direction == "desc" {
		return reverseLexicographical(value)
	}
	return sanitizeField(value)
}

func reverseLexicographical(value string) string {
	return fmt.Sprintf("~%s", value)
}

func sanitizeField(value string) string {
	santized := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "",
	).Replace(value)

	if len(santized) > 100 {
		santized = santized[:100]
	}

	return santized
}
