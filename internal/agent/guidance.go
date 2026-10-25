package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

// GuidanceCollector belongs to the coordinator, never to the agent transcript.
type GuidanceCollector struct {
	mu       sync.Mutex
	messages []string
}

func (c *GuidanceCollector) Delivered(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, message)
}
func (c *GuidanceCollector) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.messages...)
}

// GuidanceMetadata hashes the ordered UTF-8 JSON array, preserving message boundaries.
func GuidanceMetadata(messages []string, metadata map[string]string) map[string]string {
	if len(messages) == 0 {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	encoded, _ := json.Marshal(messages)
	digest := sha256.Sum256(encoded)
	metadata["operator_steered"] = "true"
	metadata["guidance_count"] = strconv.Itoa(len(messages))
	metadata["guidance_sha256"] = hex.EncodeToString(digest[:])
	return metadata
}
