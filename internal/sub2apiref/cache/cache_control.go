// Extracted from gateway_claude_oauth_body.go; only logging is omitted.
package cache

import (
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxCacheControlBlocks = 4
const cacheTTLTarget1h = "1h"

type cacheControlPath struct {
	path string
	log  string
}

func deleteJSONPathBytes(body []byte, path string) ([]byte, bool) {
	next, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body, false
	}
	return next, true
}

func collectCacheControlPaths(body []byte) (invalidThinking []cacheControlPath, messagePaths []string, toolPaths []string, systemPaths []string) {
	system := gjson.GetBytes(body, "system")
	if system.IsArray() {
		sysIndex := 0
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				path := fmt.Sprintf("system.%d.cache_control", sysIndex)
				if item.Get("type").String() == "thinking" {
					invalidThinking = append(invalidThinking, cacheControlPath{
						path: path,
						log:  "[Warning] Removed illegal cache_control from thinking block in system",
					})
				} else {
					systemPaths = append(systemPaths, path)
				}
			}
			sysIndex++
			return true
		})
	}

	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		msgIndex := 0
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				contentIndex := 0
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						path := fmt.Sprintf("messages.%d.content.%d.cache_control", msgIndex, contentIndex)
						if item.Get("type").String() == "thinking" {
							invalidThinking = append(invalidThinking, cacheControlPath{
								path: path,
								log:  fmt.Sprintf("[Warning] Removed illegal cache_control from thinking block in messages[%d].content[%d]", msgIndex, contentIndex),
							})
						} else {
							messagePaths = append(messagePaths, path)
						}
					}
					contentIndex++
					return true
				})
			}
			msgIndex++
			return true
		})
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		toolIndex := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("cache_control").Exists() {
				toolPaths = append(toolPaths, fmt.Sprintf("tools.%d.cache_control", toolIndex))
			}
			toolIndex++
			return true
		})
	}

	return invalidThinking, messagePaths, toolPaths, systemPaths
}

func enforceCacheControlLimit(body []byte) []byte {
	return normalizeCacheControlTTLOrder(enforceCacheControlBlockLimit(body))
}

func enforceCacheControlBlockLimit(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	invalidThinking, messagePaths, toolPaths, systemPaths := collectCacheControlPaths(body)
	out := body
	modified := false

	// 先清理 thinking 块中的非法 cache_control（thinking 块不支持该字段）
	for _, item := range invalidThinking {
		if !gjson.GetBytes(out, item.path).Exists() {
			continue
		}
		next, ok := deleteJSONPathBytes(out, item.path)
		if !ok {
			continue
		}
		out = next
		modified = true
	}

	count := len(messagePaths) + len(toolPaths) + len(systemPaths)
	if count <= maxCacheControlBlocks {
		if modified {
			return out
		}
		return body
	}

	// 超限：优先从 tools 中移除，再从 messages 中移除，最后才从 system 中移除。
	remaining := count - maxCacheControlBlocks
	for i := len(toolPaths) - 1; i >= 0 && remaining > 0; i-- {
		path := toolPaths[i]
		if !gjson.GetBytes(out, path).Exists() {
			continue
		}
		next, ok := deleteJSONPathBytes(out, path)
		if !ok {
			continue
		}
		out = next
		modified = true
		remaining--
	}

	for _, path := range messagePaths {
		if remaining <= 0 {
			break
		}
		if !gjson.GetBytes(out, path).Exists() {
			continue
		}
		next, ok := deleteJSONPathBytes(out, path)
		if !ok {
			continue
		}
		out = next
		modified = true
		remaining--
	}

	for i := len(systemPaths) - 1; i >= 0 && remaining > 0; i-- {
		path := systemPaths[i]
		if !gjson.GetBytes(out, path).Exists() {
			continue
		}
		next, ok := deleteJSONPathBytes(out, path)
		if !ok {
			continue
		}
		out = next
		modified = true
		remaining--
	}

	if modified {
		return out
	}
	return body
}

func normalizeCacheControlTTLOrder(body []byte) []byte {
	_, messagePaths, toolPaths, systemPaths := collectCacheControlPaths(body)
	paths := make([]string, 0, len(toolPaths)+len(systemPaths)+len(messagePaths)+1)
	paths = append(paths, toolPaths...)
	paths = append(paths, systemPaths...)
	paths = append(paths, messagePaths...)
	// A top-level cache_control applies to the last cacheable block.
	if gjson.GetBytes(body, "cache_control").Exists() {
		paths = append(paths, "cache_control")
	}

	last1h := -1
	for i, path := range paths {
		if gjson.GetBytes(body, path+".ttl").String() == cacheTTLTarget1h {
			last1h = i
		}
	}

	out := body
	for _, path := range paths[:last1h+1] {
		cc := gjson.GetBytes(out, path)
		if cc.Get("type").String() != "ephemeral" || cc.Get("ttl").String() == cacheTTLTarget1h {
			continue
		}
		if next, err := sjson.SetBytes(out, path+".ttl", cacheTTLTarget1h); err == nil {
			out = next
		}
	}
	return out
}
