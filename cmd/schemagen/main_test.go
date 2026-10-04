// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	for _, j := range jobs {
		a, err := generate("../..", j)
		if err != nil {
			t.Fatalf("%s: %v", j.schema, err)
		}
		b, err := generate("../..", j)
		if err != nil {
			t.Fatal(err)
		}
		for path := range a {
			if !bytes.Equal(a[path], b[path]) {
				t.Errorf("%s: output not deterministic", path)
			}
		}
	}
}

func TestValidateRejectsUnprefixedStorageProperty(t *testing.T) {
	s := &schemaFile{GoType: "Storage", Properties: []property{{
		Key: "remote", Field: "Remote", Type: "string", Title: "t", Description: "d",
	}}}
	if err := validate(s, true); err == nil || !strings.Contains(err.Error(), "rclone-") {
		t.Fatalf("unprefixed storage property accepted: %v", err)
	}
	if err := validate(s, false); err != nil {
		t.Fatalf("node/daemon properties need no prefix: %v", err)
	}
}

func TestValidateRejectsBadDefinitions(t *testing.T) {
	base := property{Key: "rclone-x", Field: "X", Type: "string", Title: "t", Description: "d"}
	for name, mutate := range map[string]func(*property){
		"bad pattern":         func(p *property) { p.Pattern = "(" },
		"unknown type":        func(p *property) { p.Type = "float" },
		"enum without values": func(p *property) { p.Type = "enum" },
		"bad list element":    func(p *property) { p.Type = "list"; p.Element = "node" },
		"required default":    func(p *property) { p.Option = "required"; p.Default = "x" },
		"missing description": func(p *property) { p.Description = "" },
		"bad gui":             func(p *property) { p.GUI = "secret" },
	} {
		p := base
		mutate(&p)
		if err := validate(&schemaFile{GoType: "Storage", Properties: []property{p}}, true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPerlQuote(t *testing.T) {
	if got := perlQuote(`a'b\c`); got != `'a\'b\\c'` {
		t.Fatalf("perlQuote = %s", got)
	}
}
