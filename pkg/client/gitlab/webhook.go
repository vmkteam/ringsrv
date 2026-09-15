// Package gitlab holds what this service knows about GitLab's own formats.
// Today that is one thing: the shape of a push event. It lives in a client
// package rather than in the HTTP handler because it is somebody else's
// schema, and a second forge — or GitLab changing the payload — should be an
// edit here rather than in the app's routing.
package gitlab

import (
	"encoding/json"
	"fmt"
)

// KindPush is the object_kind of a push event.
const KindPush = "push"

// PushEvent is the part of a GitLab push this service reads: which project,
// and whether it was a push at all.
type PushEvent struct {
	Kind string
	// ProjectID is what the catalogue matches against GitLabProject.
	ProjectID int
}

// ParsePush decodes a webhook body. GitLab sends the project id twice, under
// two names, and which one arrives depends on its version — so both are read
// and the nested one wins.
func ParsePush(body []byte) (PushEvent, error) {
	var raw struct {
		Kind    string `json:"object_kind"`
		Project struct {
			ID int `json:"id"`
		} `json:"project"`
		ProjectID int `json:"project_id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PushEvent{}, fmt.Errorf("gitlab: parse push event: %w", err)
	}

	id := raw.Project.ID
	if id == 0 {
		id = raw.ProjectID
	}
	return PushEvent{Kind: raw.Kind, ProjectID: id}, nil
}

// IsPush reports whether this event is one worth fetching for.
func (e PushEvent) IsPush() bool { return e.Kind == KindPush && e.ProjectID != 0 }
