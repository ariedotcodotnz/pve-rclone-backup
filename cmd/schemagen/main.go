// SPDX-License-Identifier: AGPL-3.0-or-later

// Command schemagen generates configuration code from schema/*.yaml:
// Go types and decoders, JSON Schemas, and the Perl property module of the
// storage plugin. Run it through `make generate`.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type schemaFile struct {
	GoType      string     `yaml:"go_type"`
	StorageType string     `yaml:"storage_type"`
	PerlPackage string     `yaml:"perl_package"`
	File        string     `yaml:"file"`
	BaseOptions []string   `yaml:"base_options"`
	Properties  []property `yaml:"properties"`
}

type property struct {
	Key         string   `yaml:"key"`
	Field       string   `yaml:"field"`
	Type        string   `yaml:"type"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Pattern     string   `yaml:"pattern"`
	MaxLength   int      `yaml:"max_length"`
	Values      []string `yaml:"values"`
	Element     string   `yaml:"element"`
	Minimum     *int64   `yaml:"minimum"`
	Maximum     *int64   `yaml:"maximum"`
	AllowOff    bool     `yaml:"allow_off"`
	Default     any      `yaml:"default"`
	Option      string   `yaml:"option"`
	GUI         string   `yaml:"gui"`
}

type job struct {
	schema  string // schema file name without extension
	goFile  string // output Go file
	json    string // output JSON Schema
	perl    string // output Perl module (storage only)
	doc     string // output reference page of the documentation
	storage bool
}

var jobs = []job{
	{schema: "storage", goFile: "internal/config/storage_gen.go", json: "internal/config/storage.schema.json",
		perl: "perl/PVE/Storage/Custom/RcloneBackup/Schema.pm", doc: "docs/reference/storage-properties.md", storage: true},
	{schema: "node", goFile: "internal/config/node_gen.go", json: "internal/config/node.schema.json",
		doc: "docs/reference/node-settings.md"},
	{schema: "daemon", goFile: "internal/config/daemon_gen.go", json: "internal/config/daemon.schema.json",
		doc: "docs/reference/daemon-settings.md"},
}

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "fail if generated files are out of date instead of writing them")
	flag.Parse()

	stale := false
	for _, j := range jobs {
		outputs, err := generate(*root, j)
		if err != nil {
			fmt.Fprintf(os.Stderr, "schemagen: %s: %v\n", j.schema, err)
			os.Exit(1)
		}
		for path, content := range outputs {
			full := filepath.Join(*root, path)
			if *check {
				old, _ := os.ReadFile(full)
				if !bytes.Equal(old, content) {
					fmt.Fprintf(os.Stderr, "schemagen: %s is out of date; run make generate\n", path)
					stale = true
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(full, content, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
				os.Exit(1)
			}
		}
	}
	if stale {
		os.Exit(1)
	}
}

func generate(root string, j job) (map[string][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, "schema", j.schema+".yaml"))
	if err != nil {
		return nil, err
	}
	var s schemaFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := validate(&s, j.storage); err != nil {
		return nil, err
	}

	out := map[string][]byte{}
	goSrc, err := genGo(&s, j)
	if err != nil {
		return nil, err
	}
	out[j.goFile] = goSrc
	out[j.json] = genJSONSchema(&s, j)
	if j.perl != "" {
		out[j.perl] = genPerl(&s, j)
	}
	doc, err := genDoc(&s, j)
	if err != nil {
		return nil, err
	}
	out[j.doc] = doc
	return out, nil
}

var (
	keyRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	fieldRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

func validate(s *schemaFile, storage bool) error {
	if !fieldRe.MatchString(s.GoType) {
		return fmt.Errorf("invalid go_type %q", s.GoType)
	}
	seen := map[string]bool{}
	for _, p := range s.Properties {
		if !keyRe.MatchString(p.Key) || seen[p.Key] {
			return fmt.Errorf("invalid or duplicate key %q", p.Key)
		}
		seen[p.Key] = true
		if storage && !strings.HasPrefix(p.Key, "rclone-") {
			return fmt.Errorf("storage property %q must start with rclone-", p.Key)
		}
		if !fieldRe.MatchString(p.Field) {
			return fmt.Errorf("%s: invalid field %q", p.Key, p.Field)
		}
		if p.Title == "" || p.Description == "" {
			return fmt.Errorf("%s: title and description are required", p.Key)
		}
		if p.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil {
				return fmt.Errorf("%s: pattern: %w", p.Key, err)
			}
		}
		switch p.Type {
		case "string", "boolean", "integer", "size":
		case "duration":
		case "enum":
			if len(p.Values) == 0 {
				return fmt.Errorf("%s: enum without values", p.Key)
			}
		case "list":
			if !slices.Contains([]string{"storage-id", "tag", "vmid"}, p.Element) {
				return fmt.Errorf("%s: unsupported list element %q", p.Key, p.Element)
			}
		default:
			return fmt.Errorf("%s: unsupported type %q", p.Key, p.Type)
		}
		switch p.Option {
		case "", "optional", "required", "fixed", "fixed-optional":
		default:
			return fmt.Errorf("%s: invalid option %q", p.Key, p.Option)
		}
		if !storage && p.Option != "" {
			return fmt.Errorf("%s: option is only valid for storage properties", p.Key)
		}
		if (p.Option == "required" || p.Option == "fixed") && p.Default != nil {
			return fmt.Errorf("%s: required properties cannot have a default", p.Key)
		}
		switch p.GUI {
		case "", "advanced", "hidden":
		default:
			return fmt.Errorf("%s: invalid gui %q", p.Key, p.GUI)
		}
	}
	return nil
}

func required(p property) bool { return p.Option == "required" || p.Option == "fixed" }

func defaultString(p property) string {
	switch v := p.Default.(type) {
	case nil:
		return ""
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(v)
	}
}

func goFieldType(p property) string {
	switch p.Type {
	case "boolean":
		return "bool"
	case "integer":
		return "int"
	case "size":
		return "int64"
	case "duration":
		if p.AllowOff {
			return "Interval"
		}
		return "time.Duration"
	case "list":
		if p.Element == "vmid" {
			return "[]int"
		}
		return "[]string"
	default:
		return "string"
	}
}

func reVar(p property) string {
	var b strings.Builder
	b.WriteString("re")
	for part := range strings.SplitSeq(p.Key, "-") {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func int64Ptr(v *int64) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("new(int64(%d))", *v)
}

func genGo(s *schemaFile, j job) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by schemagen from schema/%s.yaml. DO NOT EDIT.\n\n", j.schema)
	b.WriteString("package config\n\n")
	imports := []string{"errors"}
	if slices.ContainsFunc(s.Properties, func(p property) bool { return p.Pattern != "" }) {
		imports = append(imports, "regexp")
	}
	if slices.ContainsFunc(s.Properties, func(p property) bool { return p.Type == "duration" && !p.AllowOff }) {
		imports = append(imports, "time")
	}
	b.WriteString("import (\n")
	for _, imp := range imports {
		fmt.Fprintf(&b, "\t%q\n", imp)
	}
	b.WriteString(")\n\n")

	for _, p := range s.Properties {
		if p.Pattern != "" {
			fmt.Fprintf(&b, "var %s = regexp.MustCompile(%s)\n", reVar(p), strconv.Quote("^(?:"+p.Pattern+")$"))
		}
	}
	b.WriteString("\n")

	where := "the configuration"
	if s.File != "" {
		where = s.File
	}
	if s.StorageType != "" {
		where = "a " + s.StorageType + " storage"
	}
	fmt.Fprintf(&b, "// %s holds the settings of %s.\n", s.GoType, where)
	fmt.Fprintf(&b, "type %s struct {\n", s.GoType)
	if j.storage {
		b.WriteString("\t// ID is the storage ID (section name in storage.cfg).\n\tID string\n\n")
	}
	for _, p := range s.Properties {
		fmt.Fprintf(&b, "\t// %s (%s): %s\n", p.Field, p.Key, p.Description)
		fmt.Fprintf(&b, "\t%s %s\n", p.Field, goFieldType(p))
	}
	if j.storage {
		b.WriteString("\n\t// Base holds properties defined by PVE's base storage plugin.\n\tBase BaseOptions\n")
	}
	b.WriteString("}\n\n")

	// Keys.
	fmt.Fprintf(&b, "// %sKeys lists the properties defined by this schema.\n", lowerFirst(s.GoType))
	fmt.Fprintf(&b, "var %sKeys = []string{", lowerFirst(s.GoType))
	for i, p := range s.Properties {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(p.Key))
	}
	b.WriteString("}\n\n")

	// Defaults.
	fmt.Fprintf(&b, "// default%s returns the settings with every default applied.\n", s.GoType)
	fmt.Fprintf(&b, "func default%s() *%s {\n\treturn &%s{\n", s.GoType, s.GoType, s.GoType)
	for _, p := range s.Properties {
		if p.Default == nil {
			continue
		}
		ds := defaultString(p)
		var expr string
		switch p.Type {
		case "boolean":
			expr = strconv.FormatBool(ds == "1")
		case "integer":
			expr = ds
		case "size":
			expr = fmt.Sprintf("mustSize(%q)", ds)
		case "duration":
			if p.AllowOff {
				expr = fmt.Sprintf("mustInterval(%q)", ds)
			} else {
				expr = fmt.Sprintf("mustDuration(%q)", ds)
			}
		case "list":
			if p.Element == "vmid" {
				return nil, fmt.Errorf("%s: vmid list defaults are not supported", p.Key)
			}
			expr = fmt.Sprintf("SplitList(%q)", ds)
		default:
			expr = strconv.Quote(ds)
		}
		fmt.Fprintf(&b, "\t\t%s: %s,\n", p.Field, expr)
	}
	b.WriteString("\t}\n}\n\n")

	// Decoder.
	fmt.Fprintf(&b, "// decode%s decodes raw properties on top of the defaults. Every\n// problem is reported; consumed records the keys that were understood.\n", s.GoType)
	fmt.Fprintf(&b, "func decode%s(props map[string]Raw, consumed map[string]bool) (*%s, error) {\n", s.GoType, s.GoType)
	fmt.Fprintf(&b, "\tc := default%s()\n\tvar errs []error\n", s.GoType)
	b.WriteString("\tfail := func(key string, err error) { errs = append(errs, &PropertyError{Key: key, Err: err}) }\n\n")
	for _, p := range s.Properties {
		fmt.Fprintf(&b, "\tif r, ok := props[%q]; ok {\n\t\tconsumed[%q] = true\n", p.Key, p.Key)
		var call string
		switch p.Type {
		case "string":
			re := "nil"
			if p.Pattern != "" {
				re = reVar(p)
			}
			call = fmt.Sprintf("decodeString(r, %s, %d)", re, p.MaxLength)
		case "enum":
			call = fmt.Sprintf("decodeEnum(r, %#v)", p.Values)
		case "boolean":
			call = "decodeBool(r)"
		case "integer":
			call = fmt.Sprintf("decodeInt(r, %s, %s)", int64Ptr(p.Minimum), int64Ptr(p.Maximum))
		case "size":
			call = fmt.Sprintf("decodeSize(r, %s)", int64Ptr(p.Minimum))
		case "duration":
			call = fmt.Sprintf("decodeDuration(r, %t)", p.AllowOff)
		case "list":
			switch p.Element {
			case "vmid":
				call = "decodeVMIDList(r)"
			case "tag":
				call = `decodeStringList(r, ValidTag, "tag")`
			default:
				call = `decodeStringList(r, ValidStorageID, "storage ID")`
			}
		}
		fmt.Fprintf(&b, "\t\tif v, err := %s; err != nil {\n\t\t\tfail(%q, err)\n\t\t} else {\n", call, p.Key)
		switch {
		case p.Type == "integer":
			fmt.Fprintf(&b, "\t\t\tc.%s = int(v)\n", p.Field)
		case p.Type == "duration" && !p.AllowOff:
			fmt.Fprintf(&b, "\t\t\tc.%s = v.Duration\n", p.Field)
		default:
			fmt.Fprintf(&b, "\t\t\tc.%s = v\n", p.Field)
		}
		b.WriteString("\t\t}\n\t}")
		if required(p) {
			fmt.Fprintf(&b, " else {\n\t\tfail(%q, errMissing)\n\t}", p.Key)
		}
		b.WriteString("\n")
	}
	b.WriteString("\treturn c, errors.Join(errs...)\n}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated Go: %w\n%s", err, b.String())
	}
	return src, nil
}

func lowerFirst(s string) string { return strings.ToLower(s[:1]) + s[1:] }

func genJSONSchema(s *schemaFile, j job) []byte {
	props := map[string]any{}
	var req []string
	for _, p := range s.Properties {
		m := map[string]any{"title": p.Title, "description": p.Description}
		switch p.Type {
		case "boolean":
			m["type"] = "boolean"
		case "integer":
			m["type"] = "integer"
			if p.Minimum != nil {
				m["minimum"] = *p.Minimum
			}
			if p.Maximum != nil {
				m["maximum"] = *p.Maximum
			}
		case "enum":
			m["type"] = "string"
			m["enum"] = p.Values
		case "duration":
			m["type"] = "string"
			m["pattern"] = "^(?:" + perlPattern(p) + ")$"
			m["x-format"] = "duration"
		case "size":
			m["type"] = "string"
			m["pattern"] = "^(?:" + perlPattern(p) + ")$"
			m["x-format"] = "size"
			if p.Minimum != nil {
				m["x-minimum-bytes"] = *p.Minimum
			}
		case "list":
			m["type"] = "string"
			m["x-format"] = p.Element + "-list"
		default:
			m["type"] = "string"
			if p.Pattern != "" {
				m["pattern"] = "^(?:" + p.Pattern + ")$"
			}
			if p.MaxLength > 0 {
				m["maxLength"] = p.MaxLength
			}
		}
		if p.Default != nil {
			switch p.Type {
			case "boolean":
				m["default"] = defaultString(p) == "1"
			case "integer":
				n, _ := strconv.ParseInt(defaultString(p), 10, 64)
				m["default"] = n
			default:
				m["default"] = defaultString(p)
			}
		}
		if p.Option != "" {
			m["x-option"] = p.Option
		}
		if p.GUI != "" {
			m["x-gui"] = p.GUI
		}
		if required(p) {
			req = append(req, p.Key)
		}
		props[p.Key] = m
	}
	doc := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"$id":        "https://github.com/ariedotcodotnz/pve-rclone-backup/schema/" + j.schema,
		"title":      s.GoType,
		"type":       "object",
		"properties": props,
	}
	if len(req) > 0 {
		doc["required"] = req
	}
	if len(s.BaseOptions) > 0 {
		doc["x-base-options"] = s.BaseOptions
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n')
}

// perlPattern returns the validation pattern used in the Perl schema.
func perlPattern(p property) string {
	switch p.Type {
	case "duration":
		if p.AllowOff {
			return `off|0|\d+[smhdw]`
		}
		return `0|\d+[smhdw]`
	case "size":
		return `\d+[KMGT]?`
	}
	return p.Pattern
}

// sizeFormatName names the PVE schema format that checks a bounded size
// property. Format names are global in PVE, hence the storage type prefix.
func sizeFormatName(s *schemaFile, p property) string {
	return s.StorageType + "-" + strings.TrimPrefix(p.Key, "rclone-")
}

// perlSizeFormats registers a PVE schema format for every size property with
// a minimum: string properties have no numeric bounds in PVE's schema, and
// without them PVE would accept sizes the daemon rejects. PVE runs format
// checks on API parameters and on every storage.cfg line, like patterns.
func perlSizeFormats(s *schemaFile, props []property) string {
	var regs []string
	for _, p := range props {
		if p.Type == "size" && p.Minimum != nil {
			regs = append(regs, fmt.Sprintf("_register_size_format(%s, %d);", perlQuote(sizeFormatName(s, p)), *p.Minimum))
		}
	}
	if len(regs) == 0 {
		return ""
	}
	return `
use PVE::JSONSchema;

# Bytes of a size like 512M (binary units), or undef if it is invalid or
# above 2^62, as the daemon parses it.
sub size_bytes($value) {
    my ($n, $unit) = ($value // '') =~ m/^(\d+)([KMGT]?)$/ or return undef;
    my $bytes = $n * 2**{ '' => 0, K => 10, M => 20, G => 30, T => 40 }->{$unit};
    return $bytes <= 2**62 ? $bytes : undef;
}

sub _register_size_format($name, $minimum) {
    return if PVE::JSONSchema::get_format($name);
    PVE::JSONSchema::register_format($name, sub ($value, $noerr = undef) {
        my $bytes = size_bytes($value);
        return $value if defined($bytes) && $bytes >= $minimum;
        return undef if $noerr;
        die "size must be at least $minimum bytes\n";
    });
}

` + strings.Join(regs, "\n") + "\n"
}

func perlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func genPerl(s *schemaFile, j job) []byte {
	props := slices.Clone(s.Properties)
	slices.SortFunc(props, func(a, b property) int { return strings.Compare(a.Key, b.Key) })

	var b strings.Builder
	fmt.Fprintf(&b, `# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Code generated by schemagen from schema/%s.yaml. DO NOT EDIT.
#
# Storage property schema of the %s storage type. Every property defined
# here carries the "rclone-" prefix: storage.cfg shares one property
# namespace between all plugins and a duplicate name makes PVE::Storage fail
# to load on the whole node.
package %s;

use v5.36;
`, j.schema, s.StorageType, s.PerlPackage)
	b.WriteString(perlSizeFormats(s, props))
	b.WriteString(`
my $properties = {
`)
	for _, p := range props {
		fmt.Fprintf(&b, "    %s => {\n", perlQuote(p.Key))
		fmt.Fprintf(&b, "        title => %s,\n", perlQuote(p.Title))
		fmt.Fprintf(&b, "        description => %s,\n", perlQuote(p.Description))
		switch p.Type {
		case "boolean":
			b.WriteString("        type => 'boolean',\n")
		case "integer":
			b.WriteString("        type => 'integer',\n")
			if p.Minimum != nil {
				fmt.Fprintf(&b, "        minimum => %d,\n", *p.Minimum)
			}
			if p.Maximum != nil {
				fmt.Fprintf(&b, "        maximum => %d,\n", *p.Maximum)
			}
		case "enum":
			b.WriteString("        type => 'string',\n")
			quoted := make([]string, len(p.Values))
			for i, v := range p.Values {
				quoted[i] = perlQuote(v)
			}
			fmt.Fprintf(&b, "        enum => [%s],\n", strings.Join(quoted, ", "))
		case "list":
			b.WriteString("        type => 'string',\n")
			fmt.Fprintf(&b, "        format => %s,\n", perlQuote("pve-"+p.Element+"-list"))
		default:
			b.WriteString("        type => 'string',\n")
			if pat := perlPattern(p); pat != "" {
				fmt.Fprintf(&b, "        pattern => %s,\n", perlQuote("(?:"+pat+")"))
			}
			if p.Type == "size" && p.Minimum != nil {
				fmt.Fprintf(&b, "        format => %s,\n", perlQuote(sizeFormatName(s, p)))
			}
			if p.MaxLength > 0 {
				fmt.Fprintf(&b, "        maxLength => %d,\n", p.MaxLength)
			}
		}
		if p.Default != nil {
			switch p.Type {
			case "boolean", "integer":
				fmt.Fprintf(&b, "        default => %s,\n", defaultString(p))
			default:
				fmt.Fprintf(&b, "        default => %s,\n", perlQuote(defaultString(p)))
			}
		}
		b.WriteString("    },\n")
	}
	b.WriteString("};\n\nmy $options = {\n")
	for _, p := range props {
		var opt string
		switch p.Option {
		case "required":
			opt = "{}"
		case "fixed":
			opt = "{ fixed => 1 }"
		case "fixed-optional":
			opt = "{ fixed => 1, optional => 1 }"
		default:
			opt = "{ optional => 1 }"
		}
		fmt.Fprintf(&b, "    %s => %s,\n", perlQuote(p.Key), opt)
	}
	b.WriteString("    # Base properties shared with all storage plugins.\n")
	for _, o := range s.BaseOptions {
		fmt.Fprintf(&b, "    %s => { optional => 1 },\n", perlQuote(o))
	}
	b.WriteString("};\n\n")
	for _, gui := range []string{"advanced", "hidden"} {
		fmt.Fprintf(&b, "my $%s = {", gui)
		var keys []string
		for _, p := range props {
			if p.GUI == gui {
				keys = append(keys, perlQuote(p.Key)+" => 1")
			}
		}
		if len(keys) > 0 {
			b.WriteString("\n    " + strings.Join(keys, ",\n    ") + ",\n")
		}
		b.WriteString("};\n")
	}
	b.WriteString(`
sub properties { return { map { $_ => { $properties->{$_}->%* } } keys %$properties } }
sub options { return { map { $_ => { $options->{$_}->%* } } keys %$options } }
sub advanced_properties { return { %$advanced } }
sub hidden_properties { return { %$hidden } }

1;
`)
	return []byte(b.String())
}
