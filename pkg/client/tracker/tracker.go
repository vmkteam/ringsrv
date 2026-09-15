// Package tracker reads what an issue tracker answered: a parser and nothing
// else. The request is made by the layer that knows which target the caller may
// read; this turns the body into the few fields an answer about code needs.
package tracker

import (
	"encoding/json"
	"regexp"
)

// artefactRe pulls the links a /solve leaves behind out of a comment. Both
// shapes appear in practice: a bare path in the repository and a URL.
var artefactRe = regexp.MustCompile(`(?i)(?:^|[\s(])((?:https?://\S+|docs/llm/\S+?)\.md)`)

// Issue is what the tracker knows about a task.
type Issue struct {
	ID          string
	Summary     string
	Description string
	Artifacts   []string
}

// Parse reads a tracker's answer. Trackers disagree about names — YouTrack says
// summary/description/comments[].text, Jira wraps the same in fields and calls a
// comment body — so the few shapes we meet are read here rather than modelled as
// one struct that fits none of them.
//
// A body that is not an object returns nil: "the tracker did not answer about an
// issue", which the caller reports as unavailable rather than as an empty issue.
func Parse(body []byte, id string) *Issue {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	// Jira nests everything one level down.
	fields := raw
	if f, ok := raw["fields"].(map[string]any); ok {
		fields = f
	}

	issue := &Issue{
		ID:          id,
		Summary:     firstString(fields, "summary", "title", "name"),
		Description: firstString(fields, "description", "body"),
	}
	for _, text := range commentTexts(fields) {
		issue.Artifacts = append(issue.Artifacts, artefacts(text)...)
	}
	return issue
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// commentTexts finds the comment bodies wherever this tracker keeps them.
func commentTexts(fields map[string]any) []string {
	list, ok := fields["comments"].([]any)
	if !ok {
		// Jira: comment.comments[].body
		if c, ok := fields["comment"].(map[string]any); ok {
			list, _ = c["comments"].([]any)
		}
	}

	res := make([]string, 0, len(list))
	for _, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text := firstString(c, "text", "body"); text != "" {
			res = append(res, text)
		}
	}
	return res
}

// artefacts pulls research and spec links out of a comment — what a /solve
// leaves behind.
func artefacts(text string) []string {
	found := artefactRe.FindAllStringSubmatch(text, -1)
	res := make([]string, 0, len(found))
	for _, m := range found {
		res = append(res, m[1])
	}
	return res
}
