package chdb

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
)

// columnList is the "(a, b)" a column-level grant carries after its type.
var columnList = regexp.MustCompile(`\([^)]*\)`)

// readAccess are the access types a read-only user is expected to hold;
// anything else in its grants is a write or an administrative right.
var readAccess = map[string]bool{
	"SELECT": true, "SHOW": true, "SHOW TABLES": true, "SHOW COLUMNS": true,
	"SHOW DATABASES": true, "SHOW DICTIONARIES": true, "dictGet": true,
}

// ProveReadOnly runs the three probes and, on the way, learns which query
// settings the user's profile lets it change — which is what decides whether a
// query carries settings at all. The domain repeats it hourly, because the
// client has no per-connection hook.
//
// The grants come from SHOW GRANTS … FINAL rather than system.grants: that table
// needs a grant of its own, which a read-only user has no business holding,
// while SHOW GRANTS answers every user about itself and FINAL folds in its roles.
func (c *Client) ProveReadOnly(parent context.Context) (dbq.Proof, error) {
	return under(parent, c.prove)
}

func (c *Client) prove(ctx context.Context) (dbq.Proof, error) {
	rows, err := c.conn.Query(ctx, `SELECT name, value, readonly FROM system.settings WHERE name IN (?, ?, ?, ?, ?, ?)`,
		settingReadonly, settingAllowDDL, settingMaxExecutionTime, settingMaxResultRows, settingMaxResultBytes, settingResultOverflowMode)
	if err != nil {
		return dbq.Proof{}, err
	}
	defer rows.Close()

	values := map[string]string{}
	changeable := map[string]bool{}
	for rows.Next() {
		var name, value string
		var ro uint8
		if scanErr := rows.Scan(&name, &value, &ro); scanErr != nil {
			return dbq.Proof{}, scanErr
		}
		values[name] = value
		changeable[name] = ro == 0
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return dbq.Proof{}, rowsErr
	}
	c.mu.Lock()
	c.changeable = changeable
	c.mu.Unlock()

	// Only the two read-only modes prove anything; a missing row is not one.
	if ro := values[settingReadonly]; ro != "1" && ro != "2" {
		return dbq.Proof{Reason: "readonly = " + ro + ": the user may write"}, nil
	}
	if values[settingAllowDDL] != "0" {
		return dbq.Proof{Reason: "allow_ddl = " + values[settingAllowDDL] + ": the user may run DDL"}, nil
	}

	grants, err := c.conn.Query(ctx, "SHOW GRANTS FOR CURRENT_USER FINAL")
	if err != nil {
		return dbq.Proof{}, err
	}
	defer grants.Close()
	for grants.Next() {
		var line string
		if scanErr := grants.Scan(&line); scanErr != nil {
			return dbq.Proof{}, scanErr
		}
		if extra := beyondRead(line); extra != "" {
			return dbq.Proof{Reason: "user holds " + extra}, nil
		}
	}
	if rowsErr := grants.Err(); rowsErr != nil {
		return dbq.Proof{}, rowsErr
	}
	return dbq.Proof{ReadOnly: true}, nil
}

// beyondRead reads one line of SHOW GRANTS — "GRANT SELECT, INSERT ON db.* TO
// user [WITH GRANT OPTION]" — and names the first access type that is not a
// read, or the grant option, which is an administrative right in itself. REVOKE
// and role-membership lines carry nothing to judge.
func beyondRead(line string) string {
	rest, ok := strings.CutPrefix(line, "GRANT ")
	if !ok {
		return ""
	}
	types, scope, ok := strings.Cut(rest, " ON ")
	if !ok {
		return ""
	}
	target, _, _ := strings.Cut(scope, " TO ")
	if strings.HasSuffix(scope, " WITH GRANT OPTION") {
		return "GRANT OPTION on " + target
	}
	// A column list narrows a grant, SELECT(a, b): the type is the word,
	// and the commas inside the list are not the commas between types.
	for t := range strings.SplitSeq(columnList.ReplaceAllString(types, ""), ", ") {
		if !readAccess[t] {
			return t + " on " + target
		}
	}
	return ""
}

// Changeable reports which query settings the last probe found the user may
// set — what a test and a cheat sheet ask; a query asks querySettings.
func (c *Client) Changeable() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, ok := range c.changeable {
		if ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
