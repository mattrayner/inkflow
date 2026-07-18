// Package note holds small helpers for editing Obsidian-flavoured Markdown
// notes maintained by inkflow.
package note

import (
	"fmt"
	"regexp"
	"strings"
)

// UpsertMarkerBlock inserts a "## <heading>" section whose body is fenced
// by `<!-- inkflow:<markerKey>:start -->` / `:end -->` comments, replacing
// the same section if it already exists. If body is empty or whitespace-only,
// content is returned unchanged so the caller doesn't have to special-case
// the disabled-section path.
func UpsertMarkerBlock(content, heading, markerKey, body string) string {
	return UpsertMarkerBlockWithFailurePolicy(content, heading, markerKey, body, false)
}

// UpsertMarkerBlockWithFailurePolicy retains a non-error marker when a
// generated AI failure is received and preservation is enabled.
func UpsertMarkerBlockWithFailurePolicy(content, heading, markerKey, body string, preserveFailure bool) string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return content
	}
	if preserveFailure && isAIFailure(body) && hasNonErrorMarker(content, heading, markerKey) {
		return content
	}

	block := fmt.Sprintf(
		"## %s\n\n<!-- inkflow:%s:start -->\n%s\n<!-- inkflow:%s:end -->\n\n",
		heading, markerKey, body, markerKey,
	)

	pattern := regexp.MustCompile(
		`(?s)(\n?)## ` + regexp.QuoteMeta(heading) +
			`\n\n<!-- inkflow:` + regexp.QuoteMeta(markerKey) +
			`:start -->.*?<!-- inkflow:` + regexp.QuoteMeta(markerKey) +
			`:end -->\n{0,2}`,
	)
	if match := pattern.FindStringSubmatchIndex(content); match != nil {
		leadingNewline := content[match[2]:match[3]]
		return content[:match[0]] + leadingNewline + block + content[match[1]:]
	}

	if content == "" {
		return block
	}
	sep := "\n\n"
	switch {
	case strings.HasSuffix(content, "\n\n"):
		sep = ""
	case strings.HasSuffix(content, "\n"):
		sep = "\n"
	}
	return content + sep + block
}

func isAIFailure(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "_AI failed:")
}

func hasNonErrorMarker(content, heading, markerKey string) bool {
	pattern := regexp.MustCompile(
		`(?s)## ` + regexp.QuoteMeta(heading) +
			`\n\n<!-- inkflow:` + regexp.QuoteMeta(markerKey) +
			`:start -->\n(.*?)\n<!-- inkflow:` + regexp.QuoteMeta(markerKey) + `:end -->`,
	)
	match := pattern.FindStringSubmatch(content)
	return len(match) == 2 && strings.TrimSpace(match[1]) != "" && !isAIFailure(match[1])
}
