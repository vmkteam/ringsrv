package target

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Formats a target can ask for relative time in. Expanding it is a domain
// decision — what the model may write and what a target expects — so it lives
// with the catalogue rather than with the HTTP client.
const (
	TimeFormatUnix    = "unix"
	TimeFormatUnixMs  = "unix_ms"
	TimeFormatUnixNs  = "unix_ns"
	TimeFormatRFC3339 = "rfc3339"
)

// timeFormats is what a catalogue may ask for. Package-level so validation
// costs no allocation per profile.
var timeFormats = []string{TimeFormatUnix, TimeFormatUnixMs, TimeFormatUnixNs, TimeFormatRFC3339}

// Relative time the model may write instead of computing a timestamp: now,
// now-1h, now-24h, now-7d. Skills computed it in the shell with `date -v`, which
// is macOS-only, and Claude Desktop has no shell at all; expanding it here
// removes a class of answers about silently the wrong day.
const nowPrefix = "now"

// ExpandPath rewrites relative values in the query parameters this profile
// declares as time-carrying, into the format the target expects.
//
// A value that does not parse as relative time is passed through: it may be an
// absolute timestamp, and mangling it is worse than doing nothing. Order and
// repeats are preserved — re-encoding through url.Values would sort keys and
// escape the braces PromQL needs raw.
func (p *Profile) ExpandPath(rawPath string, now time.Time) (string, error) {
	if len(p.TimeParams) == 0 {
		return rawPath, nil
	}
	path, query, ok := strings.Cut(rawPath, "?")
	if !ok || query == "" {
		return rawPath, nil
	}

	parts := strings.Split(query, "&")
	for i, part := range parts {
		key, value, hasValue := strings.Cut(part, "=")
		// The key is read decoded, as CheckQuery reads it: what passed the
		// allowlist as start has to be expanded as start.
		if !hasValue || !slices.Contains(p.TimeParams, unescaped(key)) {
			continue
		}
		expanded, ok, err := expandValue(value, p.TimeFormat, now)
		if err != nil {
			return "", err
		}
		if ok {
			parts[i] = key + "=" + expanded
		}
	}
	return path + "?" + strings.Join(parts, "&"), nil
}

// expandValue reports ok=false when the value is not relative time at all.
func expandValue(value, format string, now time.Time) (string, bool, error) {
	if !strings.HasPrefix(value, nowPrefix) {
		return "", false, nil
	}
	rest := strings.TrimPrefix(value, nowPrefix)

	at := now
	if rest != "" {
		offset, ok := strings.CutPrefix(rest, "-")
		if !ok {
			return "", false, nil // now+1h and friends: not something we expand
		}
		d, ok := parseDuration(offset)
		if !ok {
			return "", false, nil
		}
		at = now.Add(-d)
	}

	out, err := formatTime(at, format)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// parseDuration reads a duration the way Prometheus writes offsets, steps and
// ranges: 15s, 5m, 1h30m, 1d, 1w. Hand-rolled because time.ParseDuration stops
// at hours. It is the one grammar for every duration the model writes, so what
// now- accepts a bound accepts too. A bare number is not a duration here.
func parseDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, false
		}
		n, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, false
		}
		s = s[i:]
		var unit time.Duration
		switch {
		case strings.HasPrefix(s, "ms"):
			unit, s = time.Millisecond, s[2:]
		case strings.HasPrefix(s, "s"):
			unit, s = time.Second, s[1:]
		case strings.HasPrefix(s, "m"):
			unit, s = time.Minute, s[1:]
		case strings.HasPrefix(s, "h"):
			unit, s = time.Hour, s[1:]
		case strings.HasPrefix(s, "d"):
			unit, s = 24*time.Hour, s[1:]
		case strings.HasPrefix(s, "w"):
			unit, s = 7*24*time.Hour, s[1:]
		default:
			return 0, false
		}
		// A duration that overflows would wrap into a negative one and pass any
		// bound.
		if n > int64(math.MaxInt64/unit) || total > math.MaxInt64-time.Duration(n)*unit {
			return 0, false
		}
		total += time.Duration(n) * unit
	}
	return total, true
}

func formatTime(at time.Time, format string) (string, error) {
	switch format {
	case TimeFormatUnix, "":
		return strconv.FormatInt(at.Unix(), 10), nil
	case TimeFormatUnixMs:
		return strconv.FormatInt(at.UnixMilli(), 10), nil
	case TimeFormatUnixNs:
		return strconv.FormatInt(at.UnixNano(), 10), nil
	case TimeFormatRFC3339:
		return at.UTC().Format(time.RFC3339), nil
	default:
		return "", fmt.Errorf("upstream: unknown TimeFormat %q", format)
	}
}

// bodyTimeParam is one compiled TimeBodyParams entry: the path ExpandBody
// walks and the format the leaf at its end is written in.
type bodyTimeParam struct {
	path   []string
	format string
}

// timeFormatSep separates a path from the format it wants:
// "params.filter.from:rfc3339". A JSON key with a colon would collide, but no
// upstream in the catalogue has one, and a second parallel list is worse.
const timeFormatSep = ":"

// compileBodyTimeParams splits the profile's entries into a path and the format
// its leaf takes, and reports what is wrong with them.
//
// An entry may name its own format after a colon, because one upstream can want
// two: topsrv takes the metric window as unix integers and the weblog filter
// window as RFC3339 strings, both behind the same path. Without a suffix the
// entry takes def.
func compileBodyTimeParams(entries []string, def string) ([]bodyTimeParam, []error) {
	if len(entries) == 0 {
		return nil, nil
	}
	var errs []error
	out := make([]bodyTimeParam, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		path, format := cutTimeFormat(entry, def)
		if !slices.Contains(timeFormats, format) {
			errs = append(errs, fmt.Errorf("TimeBodyParams %q: want one of %s", entry, strings.Join(timeFormats, ", ")))
			continue
		}
		if slices.Contains(strings.Split(path, "."), "") {
			errs = append(errs, fmt.Errorf("TimeBodyParams %q: want a dotted path like params.start", entry))
			continue
		}
		// Two formats for one path would silently leave the last one standing,
		// and a window in the wrong format reads as a broken upstream.
		if seen[path] {
			errs = append(errs, fmt.Errorf("TimeBodyParams lists %q twice", path))
			continue
		}
		seen[path] = true
		out = append(out, bodyTimeParam{path: strings.Split(path, "."), format: format})
	}
	return out, errs
}

// cutTimeFormat reads the optional format suffix off an entry. Without one the
// entry takes def; a suffix that is not a format stays in the path, so the
// caller reports the whole entry as broken rather than a path it never wrote.
func cutTimeFormat(entry, def string) (path, format string) {
	path, format, ok := strings.Cut(entry, timeFormatSep)
	if !ok {
		return entry, def
	}
	return path, format
}

// ExpandBody is ExpandPath for an API whose window travels in the body: a
// JSON-RPC metric.queryRange takes start and end as integers, and "now-1h"
// written there is an unmarshal error the model reads as a broken server.
//
// The unix formats become JSON numbers, rfc3339 stays a string, and a batch gets
// every element expanded. A body that is not JSON, or a path holding something
// that is not relative time, is left as written. The body is only re-encoded
// when something changed, so an untouched request travels byte for byte.
func (p *Profile) ExpandBody(body string, now time.Time) (string, error) {
	if len(p.bodyTimeParams) == 0 || strings.TrimSpace(body) == "" {
		return body, nil
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber() // an id past 2^53 has to come back as written
	var v any
	if err := dec.Decode(&v); err != nil {
		return body, nil //nolint:nilerr // not JSON: passed on as written, the upstream says what is wrong with it
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return body, nil // trailing content: not one JSON value, not ours to rewrite
	}

	changed := false
	apply := func(obj map[string]any) error {
		for _, param := range p.bodyTimeParams {
			ok, err := expandAt(obj, param, now)
			if err != nil {
				return err
			}
			changed = changed || ok
		}
		return nil
	}
	switch node := v.(type) {
	case map[string]any:
		if err := apply(node); err != nil {
			return "", err
		}
	case []any:
		for _, el := range node {
			if obj, ok := el.(map[string]any); ok {
				if err := apply(obj); err != nil {
					return "", err
				}
			}
		}
	}
	if !changed {
		return body, nil
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// PromQL in the body compares with < and >: escaping them is valid JSON and
	// makes the audit line unreadable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("body: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// expandAt walks obj along the parameter's path and replaces the leaf when
// it is relative time. A missing step or a leaf that is not a string means
// nothing to do.
func expandAt(obj map[string]any, param bodyTimeParam, now time.Time) (bool, error) {
	for _, key := range param.path[:len(param.path)-1] {
		next, ok := obj[key].(map[string]any)
		if !ok {
			return false, nil
		}
		obj = next
	}
	leaf := param.path[len(param.path)-1]
	s, ok := obj[leaf].(string)
	if !ok {
		return false, nil
	}
	out, ok, err := expandValue(s, param.format, now)
	if err != nil || !ok {
		return false, err
	}
	if param.format == TimeFormatRFC3339 {
		obj[leaf] = out
	} else {
		obj[leaf] = json.Number(out)
	}
	return true, nil
}
