package kb

import (
	"fmt"
	"strings"
)

// Table is the note on one table: schema/<source>/<table>.md. The frontmatter
// carries what db_introspect adds to the schema the database reports — what the
// table is for, what one row is, what the columns mean. The database knows the
// shape; the note knows the meaning, and can go stale, which is why the two
// travel in separate fields.
type Table struct {
	Table     string   `yaml:"table"`
	Source    string   `yaml:"source"`
	Purpose   string   `yaml:"purpose"`
	Grain     string   `yaml:"grain"`
	Columns   []Column `yaml:"columns"`
	Notes     Values   `yaml:"notes"`
	Relations Values   `yaml:"relations"`

	// Target is the database target Source maps onto.
	Target string `yaml:"-"`
	// Topic is the help name of the file, for the full text.
	Topic string `yaml:"-"`
}

// Column is the note on one column.
type Column struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Enum        Values `yaml:"enum"`
	Rel         string `yaml:"rel"`
}

// Values is a list of scalars read as text. An enum of status codes is written
// as numbers as often as an enum of names is written as words, and a strict
// []string would refuse the first.
type Values []string

// UnmarshalYAML takes a list of any scalars, or a single scalar.
func (v *Values) UnmarshalYAML(unmarshal func(any) error) error {
	var list []any
	if err := unmarshal(&list); err != nil {
		var one any
		if err := unmarshal(&one); err != nil {
			return err
		}
		list = []any{one}
	}
	out := make(Values, 0, len(list))
	for _, x := range list {
		switch x.(type) {
		case map[any]any, []any:
			return fmt.Errorf("a list of values holds %T", x)
		case nil:
			out = append(out, "")
		default:
			out = append(out, fmt.Sprint(x))
		}
	}
	*v = out
	return nil
}

// Column returns the note on a column, matched without regard to case.
func (t *Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// Unmatched names the columns of the note that the table does not have. A note
// outlives a migration: a renamed column keeps its description under the old
// name, and the model should see that the two disagree.
func (t *Table) Unmatched(columns []string) []string {
	var out []string
	for _, c := range t.Columns {
		found := false
		for _, have := range columns {
			if strings.EqualFold(have, c.Name) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, c.Name)
		}
	}
	return out
}

// Tables returns the notes on the tables of one database target.
func (p *Pack) Tables(target string) []*Table {
	return p.tables[strings.ToLower(target)]
}

// Annotation finds the note on a table of a target. Names are matched whole
// first. Where one side leaves the schema out, the bare names may meet, and only
// when exactly one note fits: a base with several schemas can hold two tables of
// one name, and a guess would describe the wrong one. Two qualified names are
// never matched by their last part — billing.orders is not public.orders.
func (p *Pack) Annotation(target, table string) (*Table, bool) {
	notes := p.tables[strings.ToLower(target)]
	want := strings.ToLower(table)
	for _, t := range notes {
		if strings.ToLower(t.Table) == want {
			return t, true
		}
	}
	bare := lastPart(want)
	qualified := bare != want
	var hit *Table
	for _, t := range notes {
		name := strings.ToLower(t.Table)
		if qualified && lastPart(name) != name {
			continue // both qualified: compared whole above
		}
		if lastPart(name) != bare {
			continue
		}
		if hit != nil {
			return nil, false
		}
		hit = t
	}
	return hit, hit != nil
}

func lastPart(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
