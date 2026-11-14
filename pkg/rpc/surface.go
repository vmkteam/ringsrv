package rpc

import (
	"context"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/md"

	"github.com/vmkteam/embedlog"
)

// surface is what the instance can serve, whatever the role: the families of
// tools its catalogue and its wiring hold. The built-in texts — the help
// description, the prompts, the sheets in resources/list — follow it: a
// sentence, a scenario or a sheet about a family the instance does not have is
// a promise it cannot keep, and on an instance of databases alone the model
// would go looking for api_call because the help told it to.
//
// Roles do not enter here. What a role may call is filtered where it always was;
// this is the narrower question of what exists at all.
type surface struct {
	api, code, db, files bool
	// sheets are the target sheet names the catalogue's profiles read: a write
	// profile reads its read twin's sheet.
	sheets map[string]bool
}

// Families of the built-in texts, as a prompt's requires: names them. The
// three with a sheet of their own share the sheet's name.
const (
	familyAPI   = md.FamilyAPICall
	familyCode  = md.FamilyCode
	familyDB    = md.FamilyDB
	familyFiles = md.FamilyFiles
)

func (s ToolsService) surface() surface {
	out := surface{sheets: map[string]bool{}}
	if s.targets == nil {
		return out
	}
	for name := range s.targets.Profiles {
		out.api = true
		out.sheets[strings.TrimSuffix(name, "-rw")] = true
		if s.servesFiles(name) {
			out.files = true
		}
	}
	out.code = s.code.Available() && len(s.targets.Repos) > 0
	out.db = s.db != nil && len(s.targets.Databases) > 0
	return out
}

// has reports whether the instance serves a family.
func (u surface) has(family string) bool {
	switch family {
	case familyAPI:
		return u.api
	case familyCode:
		return u.code
	case familyDB:
		return u.db
	case familyFiles:
		return u.files
	}
	return false
}

// families lists the tool families with a sheet of their own, in the order the
// help text names them.
func (u surface) families() []string {
	var out []string
	for _, f := range []string{familyCode, familyDB, familyFiles} {
		if u.has(f) {
			out = append(out, f)
		}
	}
	return out
}

// keepsPrompt reports whether the instance serves every family a built-in
// prompt requires. A prompt the requirements do not name is kept: the tree's
// test makes every one of them say.
func (u surface) keepsPrompt(requires map[string][]string) func(name string) bool {
	return func(name string) bool {
		for _, f := range requires[name] {
			if !u.has(f) {
				return false
			}
		}
		return true
	}
}

// keepsResource reports whether a built-in sheet is about something the
// instance has: a target sheet for a profile of its catalogue, a tool sheet for
// a family it serves. This is the catalogue's question, not a role's — the
// listing is the same for everyone on the instance, as it always was; it just
// stops listing fourteen sheets on an instance without a single profile.
func (u surface) keepsResource(uri string) bool {
	rel := strings.TrimPrefix(uri, md.URIScheme)
	dir, file, ok := strings.Cut(rel, "/")
	if !ok {
		return true
	}
	name := strings.TrimSuffix(file, ".md")
	switch dir {
	case sheetDirTargets:
		return u.sheets[name]
	case sheetDirTools:
		return u.has(name)
	}
	return true
}

// The two directories of built-in sheets, as sheetURI writes them.
const (
	sheetDirTargets = "targets"
	sheetDirTools   = "tools"
)

// builtinFilter is what of the built-in tree this instance serves: the sheets
// about what it has, and the prompts whose requirements it meets. A tree whose
// requirements cannot be read keeps every prompt — the tree's own test is what
// fails then.
func (u surface) builtinFilter(l embedlog.Logger) builtinFilter {
	out := builtinFilter{resource: u.keepsResource}
	requires, err := md.PromptRequires()
	if err != nil {
		l.Error(context.Background(), "built-in prompts: cannot read requires", "err", err.Error())
		return out
	}
	out.prompt = u.keepsPrompt(requires)
	return out
}
