package kb

import (
	"fmt"
	"sort"
	"strings"
)

// Row is one line of the listing help() hands out: a topic, or a directory
// folded into one line.
type Row struct {
	// Name is a topic name, or a directory ending in "/".
	Name        string
	Description string
	// Count is how many topics a folded directory holds; zero on a topic.
	Count int
}

// Listing is what help() shows of the pack: the topics a caller may read, with
// every directory of more than FoldOver entries folded into one row. A listing
// is read whole by the model, and a hundred canned queries at the top level
// would cost every call that only wanted the rules. The folded directory is
// one help(["dir/"]) away.
func (p *Pack) Listing(visible func(Topic) bool) []Row {
	byDir := map[string][]Topic{}
	for _, t := range p.topics {
		if visible != nil && !visible(t) {
			continue
		}
		dir := parentDir(t.Name)
		byDir[dir] = append(byDir[dir], t)
	}
	var rows []Row
	for dir, topics := range byDir {
		if dir == "" || len(topics) <= p.opts.FoldOver {
			for _, t := range topics {
				rows = append(rows, Row{Name: t.Name, Description: t.Description})
			}
			continue
		}
		rows = append(rows, Row{Name: dir + "/", Description: dirDescription(dir, topics), Count: len(topics)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// dirDescription is what a folded directory says about itself: the description
// of its index or README when it has one, its size otherwise.
func dirDescription(dir string, topics []Topic) string {
	for _, t := range topics {
		base := strings.ToLower(strings.TrimPrefix(t.Name, dir+"/"))
		if (base == "index" || base == "readme") && t.Description != "" {
			return t.Description
		}
	}
	return fmt.Sprintf("записей: %d — help([%q]) покажет их", len(topics), dir+"/")
}
