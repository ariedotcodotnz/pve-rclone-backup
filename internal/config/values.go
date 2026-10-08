// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Raw is an undecoded property value. NoValue is set for a key that
// appeared without a value, which PVE treats as boolean true.
type Raw struct {
	Value   string
	NoValue bool
}

// PropertyError describes an invalid or missing property.
type PropertyError struct {
	Key string
	Err error
}

func (e *PropertyError) Error() string { return fmt.Sprintf("%s: %v", e.Key, e.Err) }
func (e *PropertyError) Unwrap() error { return e.Err }

var (
	errMissing    = errors.New("required property is missing")
	errNoValue    = errors.New("property has no value")
	errLineFeed   = errors.New("property contains a line feed")
	errNotPattern = errors.New("value does not match the required pattern")
)

func checkRaw(r Raw) error {
	if r.NoValue {
		return errNoValue
	}
	if strings.ContainsAny(r.Value, "\r\n") {
		return errLineFeed
	}
	return nil
}

func decodeString(r Raw, re *regexp.Regexp, maxLen int) (string, error) {
	if err := checkRaw(r); err != nil {
		return "", err
	}
	if maxLen > 0 && len(r.Value) > maxLen {
		return "", fmt.Errorf("value is longer than %d characters", maxLen)
	}
	if re != nil && !re.MatchString(r.Value) {
		return "", errNotPattern
	}
	return r.Value, nil
}

func decodeEnum(r Raw, values []string) (string, error) {
	if err := checkRaw(r); err != nil {
		return "", err
	}
	if !slices.Contains(values, r.Value) {
		return "", fmt.Errorf("value %q is not one of %s", r.Value, strings.Join(values, ", "))
	}
	return r.Value, nil
}

// decodeBool follows PVE: a bare key means true; otherwise only 0 and 1.
func decodeBool(r Raw) (bool, error) {
	if r.NoValue {
		return true, nil
	}
	switch r.Value {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("boolean must be 0 or 1, got %q", r.Value)
}

var integerRe = regexp.MustCompile(`^[+-]?\d+$`)

// decodeInt parses an integer setting into an int, the type settings are
// held in, so that no narrowing conversion follows (strconv.Atoi checks
// the range of int).
func decodeInt(r Raw, minimum, maximum *int64) (int, error) {
	if err := checkRaw(r); err != nil {
		return 0, err
	}
	if !integerRe.MatchString(r.Value) {
		return 0, fmt.Errorf("not an integer: %q", r.Value)
	}
	v, err := strconv.Atoi(r.Value)
	if err != nil {
		return 0, fmt.Errorf("integer out of range: %q", r.Value)
	}
	if minimum != nil && int64(v) < *minimum {
		return 0, fmt.Errorf("value must be at least %d", *minimum)
	}
	if maximum != nil && int64(v) > *maximum {
		return 0, fmt.Errorf("value must be at most %d", *maximum)
	}
	return v, nil
}

var durationRe = regexp.MustCompile(`^(?:0|(\d+)([smhdw]))$`)

// ParseDuration parses "0" or a number with one of the units s, m, h, d, w.
func ParseDuration(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30m, 12h, 7d, 2w or 0)", s)
	}
	if m[1] == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	unit := map[string]time.Duration{
		"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
	}[m[2]]
	if n > int64(1<<62)/int64(unit) {
		return 0, fmt.Errorf("duration %q is too long", s)
	}
	return time.Duration(n) * unit, nil
}

// Interval is a duration that may be switched off.
type Interval struct {
	Off      bool
	Duration time.Duration
}

func decodeDuration(r Raw, allowOff bool) (Interval, error) {
	if err := checkRaw(r); err != nil {
		return Interval{}, err
	}
	if allowOff && r.Value == "off" {
		return Interval{Off: true}, nil
	}
	d, err := ParseDuration(r.Value)
	return Interval{Duration: d}, err
}

var sizeRe = regexp.MustCompile(`^(\d+)([KMGT]?)$`)

// ParseSize parses a byte size with an optional binary unit K, M, G or T.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 512M, 1G)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n << shift, nil
}

func decodeSize(r Raw, minimum *int64) (int64, error) {
	if err := checkRaw(r); err != nil {
		return 0, err
	}
	n, err := ParseSize(r.Value)
	if err != nil {
		return 0, err
	}
	if minimum != nil && n < *minimum {
		return 0, fmt.Errorf("size must be at least %d bytes", *minimum)
	}
	return n, nil
}

// SplitList splits a PVE list value on commas, semicolons, whitespace or
// NUL bytes (PVE::ParseUtils::split_list).
func SplitList(s string) []string {
	if strings.Contains(s, "\x00") {
		return strings.Split(s, "\x00")
	}
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
	})
}

var (
	storageIDRe = regexp.MustCompile(`^(?i:[a-z][a-z0-9\-_.]*[a-z0-9])$`)
	tagRe       = regexp.MustCompile(`^(?i:[a-z0-9_][a-z0-9_\-+.]*)$`)
	vmidRe      = regexp.MustCompile(`^[1-9][0-9]{2,8}$`)
	nodeNameRe  = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9\-]*[a-zA-Z0-9])?$`)
)

// ValidStorageID reports whether s is a valid PVE storage ID.
func ValidStorageID(s string) bool { return len(s) >= 2 && storageIDRe.MatchString(s) }

// ValidTag reports whether s is a valid PVE guest tag.
func ValidTag(s string) bool { return tagRe.MatchString(s) }

// ValidVMID reports whether s is a valid PVE guest ID.
func ValidVMID(s string) bool { return vmidRe.MatchString(s) }

// ValidNodeName reports whether s is a valid PVE node name.
func ValidNodeName(s string) bool { return nodeNameRe.MatchString(s) }

func decodeStringList(r Raw, valid func(string) bool, what string) ([]string, error) {
	if err := checkRaw(r); err != nil {
		return nil, err
	}
	items := SplitList(r.Value)
	for _, it := range items {
		if !valid(it) {
			return nil, fmt.Errorf("invalid %s %q", what, it)
		}
	}
	return items, nil
}

func decodeVMIDList(r Raw) ([]int, error) {
	items, err := decodeStringList(r, ValidVMID, "guest ID")
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(items))
	for _, it := range items {
		n, _ := strconv.Atoi(it) // validated above
		out = append(out, n)
	}
	return out, nil
}

func mustDuration(s string) time.Duration {
	d, err := ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

func mustInterval(s string) Interval {
	iv, err := decodeDuration(Raw{Value: s}, true)
	if err != nil {
		panic(err)
	}
	return iv
}

func mustSize(s string) int64 {
	n, err := ParseSize(s)
	if err != nil {
		panic(err)
	}
	return n
}

var errUnexpected = errors.New("unexpected property")
