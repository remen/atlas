// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/specutil"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

type (
	// TextSearchConfig describes a PostgreSQL text search configuration.
	// Configurations created with COPY are represented by their resulting parser
	// and mappings, because PostgreSQL does not retain the source configuration.
	TextSearchConfig struct {
		schema.Object
		Name   string
		Schema *schema.Schema
		// Parser is a PostgreSQL identifier, optionally qualified with a schema.
		Parser   string
		Mappings []*TextSearchMapping
		Attrs    []schema.Attr
	}
	// TextSearchMapping maps one parser token type to an ordered list of dictionaries.
	TextSearchMapping struct {
		Token string
		// Dictionaries are PostgreSQL identifiers, optionally schema-qualified.
		// Order matters: dictionaries are consulted from left to right.
		Dictionaries []string
	}
	textSearchConfig struct {
		Name      string               `spec:",name"`
		Qualifier string               `spec:",qualifier"`
		Schema    *schemahcl.Ref       `spec:"schema"`
		Parser    string               `spec:"parser"`
		Mappings  []*textSearchMapping `spec:"mapping"`
		schemahcl.DefaultExtension
	}
	textSearchMapping struct {
		Token        string   `spec:",name"`
		Dictionaries []string `spec:"dictionaries"`
	}
)

func (c *TextSearchConfig) SpecType() string { return "text_search_config" }
func (c *TextSearchConfig) SpecName() string { return c.Name }

// DependsOn orders drops after tables that might reference the configuration
// in expressions. These references are not parsed by the package driver.
func (c *TextSearchConfig) DependsOn(change, other schema.Change) bool {
	if _, ok := change.(*schema.DropObject); ok {
		switch other.(type) {
		case *schema.DropTable, *schema.ModifyTable:
			return true
		}
	}
	return false
}

// DependencyOf orders configuration changes before tables that may reference it.
func (c *TextSearchConfig) DependencyOf(change, other schema.Change) bool {
	switch change.(type) {
	case *schema.AddObject, *schema.ModifyObject:
		switch other.(type) {
		case *schema.AddTable, *schema.ModifyTable:
			return true
		}
	}
	return false
}

func (c *textSearchConfig) Label() string             { return c.Name }
func (c *textSearchConfig) QualifierLabel() string    { return c.Qualifier }
func (c *textSearchConfig) SetQualifier(q string)     { c.Qualifier = q }
func (c *textSearchConfig) SchemaRef() *schemahcl.Ref { return c.Schema }

func convertTextSearchConfigs(specs []*textSearchConfig, r *schema.Realm) error {
	for _, spec := range specs {
		ns, err := specutil.SchemaName(spec.Schema)
		if err != nil {
			return fmt.Errorf("postgres: text search configuration %q: %w", spec.Name, err)
		}
		s, ok := r.Schema(ns)
		if !ok {
			return fmt.Errorf("postgres: schema %q for text search configuration %q was not found", ns, spec.Name)
		}
		if _, ok := s.Object(func(o schema.Object) bool { c, ok := o.(*TextSearchConfig); return ok && c.Name == spec.Name }); ok {
			return fmt.Errorf("postgres: duplicate text search configuration %q.%q", ns, spec.Name)
		}
		c := &TextSearchConfig{Name: spec.Name, Schema: s, Parser: spec.Parser}
		for _, m := range spec.Mappings {
			c.Mappings = append(c.Mappings, &TextSearchMapping{Token: m.Token, Dictionaries: m.Dictionaries})
		}
		if a, ok := spec.Attr("comment"); ok {
			v, err := a.String()
			if err != nil {
				return err
			}
			c.Attrs = append(c.Attrs, &schema.Comment{Text: v})
		}
		if err := validateTextSearchConfig(c); err != nil {
			return err
		}
		s.AddObjects(c)
	}
	return nil
}

func textSearchConfigSpec(c *TextSearchConfig, ns string) *textSearchConfig {
	spec := &textSearchConfig{Name: c.Name, Schema: specutil.SchemaRef(ns), Parser: c.Parser}
	for _, m := range c.Mappings {
		spec.Mappings = append(spec.Mappings, &textSearchMapping{Token: m.Token, Dictionaries: m.Dictionaries})
	}
	var cm schema.Comment
	if sqlx.Has(c.Attrs, &cm) {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.StringAttr("comment", cm.Text))
	}
	return spec
}

func (i *inspect) inspectTextSearchConfigs(ctx context.Context, r *schema.Realm) error {
	args := make([]any, 0, len(r.Schemas))
	for _, s := range r.Schemas {
		args = append(args, s.Name)
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := i.QueryContext(ctx, fmt.Sprintf(textSearchConfigsQuery, nArgs(0, len(args))), args...)
	if err != nil {
		return fmt.Errorf("postgres: querying text search configurations: %w", err)
	}
	defer rows.Close()
	var c *TextSearchConfig
	var prev int64
	for rows.Next() {
		var id int64
		var ns, name, parser string
		var comment, token, dict sql.NullString
		if err := rows.Scan(&id, &ns, &name, &parser, &comment, &token, &dict); err != nil {
			return fmt.Errorf("postgres: scanning text search configuration: %w", err)
		}
		if c == nil || id != prev {
			s, ok := r.Schema(ns)
			if !ok {
				return fmt.Errorf("postgres: schema %q for text search configuration %q was not found", ns, name)
			}
			c = &TextSearchConfig{Name: name, Schema: s, Parser: parser}
			if comment.Valid {
				c.Attrs = append(c.Attrs, &schema.Comment{Text: comment.String})
			}
			s.AddObjects(c)
			prev = id
		}
		if token.Valid {
			var m *TextSearchMapping
			if n := len(c.Mappings); n > 0 && c.Mappings[n-1].Token == token.String {
				m = c.Mappings[n-1]
			} else {
				m = &TextSearchMapping{Token: token.String}
				c.Mappings = append(c.Mappings, m)
			}
			m.Dictionaries = append(m.Dictionaries, dict.String)
		}
	}
	return rows.Err()
}

// Exclude extension members; they are managed by their extension.
const textSearchConfigsQuery = `SELECT c.oid, n.nspname, c.cfgname,
 quote_ident(pn.nspname) || '.' || quote_ident(p.prsname),
 obj_description(c.oid, 'pg_ts_config'), t.alias,
 quote_ident(dn.nspname) || '.' || quote_ident(d.dictname)
FROM pg_ts_config c
JOIN pg_namespace n ON n.oid = c.cfgnamespace
JOIN pg_ts_parser p ON p.oid = c.cfgparser
JOIN pg_namespace pn ON pn.oid = p.prsnamespace
LEFT JOIN pg_ts_config_map m ON m.mapcfg = c.oid
LEFT JOIN LATERAL ts_token_type(c.cfgparser) t ON t.tokid = m.maptokentype
LEFT JOIN pg_ts_dict d ON d.oid = m.mapdict
LEFT JOIN pg_namespace dn ON dn.oid = d.dictnamespace
WHERE n.nspname IN (%s)
 AND NOT EXISTS (SELECT 1 FROM pg_depend WHERE classid = 'pg_ts_config'::regclass AND objid = c.oid AND deptype = 'e')
ORDER BY n.nspname, c.cfgname, t.alias, m.mapseqno`

// Parser and dictionary references must be SQL identifiers.
var textSearchName = regexp.MustCompile(`^(?:[\pL_][\pL\pN_$]*|"(?:[^"]|"")+")(?:\.(?:[\pL_][\pL\pN_$]*|"(?:[^"]|"")+"))?$`)

func textSearchRef(name string) (string, error) {
	if !textSearchName.MatchString(name) {
		return "", fmt.Errorf("postgres: invalid text search identifier %q", name)
	}
	var parts []string
	for len(name) > 0 {
		var part string
		if name[0] == '"' {
			var b strings.Builder
			for j := 1; j < len(name); j++ {
				if name[j] == '"' {
					if j+1 < len(name) && name[j+1] == '"' {
						b.WriteByte('"')
						j++
						continue
					}
					part = b.String()
					name = name[j+1:]
					break
				}
				b.WriteByte(name[j])
			}
		} else {
			j := strings.IndexByte(name, '.')
			if j < 0 {
				j = len(name)
			}
			part = strings.ToLower(name[:j])
			name = name[j:]
		}
		parts = append(parts, `"`+strings.ReplaceAll(part, `"`, `""`)+`"`)
		if strings.HasPrefix(name, ".") {
			name = name[1:]
		}
	}
	return strings.Join(parts, "."), nil
}

func validateTextSearchConfig(c *TextSearchConfig) error {
	if c.Name == "" {
		return fmt.Errorf("postgres: empty text search configuration name")
	}
	if _, err := textSearchRef(c.Parser); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, m := range c.Mappings {
		if m == nil || m.Token == "" {
			return fmt.Errorf("postgres: empty text search mapping token in %q", c.Name)
		}
		if seen[m.Token] {
			return fmt.Errorf("postgres: duplicate text search mapping token %q", m.Token)
		}
		seen[m.Token] = true
		if len(m.Dictionaries) == 0 {
			return fmt.Errorf("postgres: text search mapping %q has no dictionaries", m.Token)
		}
		for _, d := range m.Dictionaries {
			if _, err := textSearchRef(d); err != nil {
				return err
			}
		}
	}
	return nil
}

func textSearchMappings(c *TextSearchConfig) map[string][]string {
	m := make(map[string][]string)
	for _, x := range c.Mappings {
		for _, d := range x.Dictionaries {
			v, _ := textSearchRef(d)
			m[x.Token] = append(m[x.Token], v)
		}
	}
	return m
}

func textSearchComment(c *TextSearchConfig) string {
	var cm schema.Comment
	sqlx.Has(c.Attrs, &cm)
	return cm.Text
}

func textSearchConfigDiff(from, to *schema.Schema) ([]schema.Change, error) {
	var changes []schema.Change
	find := func(s *schema.Schema, name string) *TextSearchConfig {
		o, ok := s.Object(func(o schema.Object) bool { c, ok := o.(*TextSearchConfig); return ok && c.Name == name })
		if !ok {
			return nil
		}
		return o.(*TextSearchConfig)
	}
	for _, o := range from.Objects {
		c1, ok := o.(*TextSearchConfig)
		if !ok {
			continue
		}
		if err := validateTextSearchConfig(c1); err != nil {
			return nil, err
		}
		c2 := find(to, c1.Name)
		if c2 == nil {
			changes = append(changes, &schema.DropObject{O: c1})
			continue
		}
		if err := validateTextSearchConfig(c2); err != nil {
			return nil, err
		}
		p1, _ := textSearchRef(c1.Parser)
		p2, _ := textSearchRef(c2.Parser)
		if p1 != p2 || !reflect.DeepEqual(textSearchMappings(c1), textSearchMappings(c2)) || textSearchComment(c1) != textSearchComment(c2) {
			changes = append(changes, &schema.ModifyObject{From: c1, To: c2})
		}
	}
	for _, o := range to.Objects {
		c, ok := o.(*TextSearchConfig)
		if !ok {
			continue
		}
		if err := validateTextSearchConfig(c); err != nil {
			return nil, err
		}
		if find(from, c.Name) == nil {
			changes = append(changes, &schema.AddObject{O: c})
		}
	}
	return changes, nil
}

func (s *state) textSearchIdent(c *TextSearchConfig) string {
	// Builder.Ident expects quotes to be escaped by the caller.
	var ns *schema.Schema
	if c.Schema != nil {
		ns = schema.New(strings.ReplaceAll(c.Schema.Name, `"`, `""`))
	}
	b := s.Build()
	if b.Schema != nil {
		q := strings.ReplaceAll(*b.Schema, `"`, `""`)
		b.Schema = &q
	}
	return b.SchemaResource(ns, strings.ReplaceAll(c.Name, `"`, `""`)).String()
}

func (s *state) textSearchCreate(c *TextSearchConfig) ([]string, error) {
	if err := validateTextSearchConfig(c); err != nil {
		return nil, err
	}
	parser, _ := textSearchRef(c.Parser)
	cmds := []string{fmt.Sprintf("CREATE TEXT SEARCH CONFIGURATION %s (PARSER = %s)", s.textSearchIdent(c), parser)}
	for _, m := range c.Mappings {
		cmds = append(cmds, s.textSearchMapping(c, m.Token, m.Dictionaries))
	}
	if cm := textSearchComment(c); cm != "" {
		cmds = append(cmds, s.textSearchComment(c, cm))
	}
	return cmds, nil
}

func (s *state) textSearchMapping(c *TextSearchConfig, token string, dicts []string) string {
	base := "ALTER TEXT SEARCH CONFIGURATION " + s.textSearchIdent(c)
	token = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	if len(dicts) == 0 {
		return base + " DROP MAPPING FOR " + token
	}
	ds := make([]string, len(dicts))
	for i, d := range dicts {
		ds[i], _ = textSearchRef(d)
	}
	return base + " ALTER MAPPING FOR " + token + " WITH " + strings.Join(ds, ", ")
}

func (s *state) textSearchComment(c *TextSearchConfig, comment string) string {
	return "COMMENT ON TEXT SEARCH CONFIGURATION " + s.textSearchIdent(c) + " IS '" + strings.ReplaceAll(comment, "'", "''") + "'"
}

func (s *state) addTextSearchConfig(add *schema.AddObject, c *TextSearchConfig) error {
	cmds, err := s.textSearchCreate(c)
	if err != nil {
		return err
	}
	for i, cmd := range cmds {
		var reverse, comment string
		if i == 0 {
			reverse = "DROP TEXT SEARCH CONFIGURATION " + s.textSearchIdent(c)
			comment = fmt.Sprintf("create text search configuration %q", c.Name)
		} else if i <= len(c.Mappings) {
			reverse = s.textSearchMapping(c, c.Mappings[i-1].Token, nil)
			comment = fmt.Sprintf("set text search configuration %q mapping %q", c.Name, c.Mappings[i-1].Token)
		} else {
			reverse = s.textSearchComment(c, "")
			comment = fmt.Sprintf("set text search configuration %q comment", c.Name)
		}
		s.append(&migrate.Change{Source: add, Cmd: cmd, Reverse: reverse, Comment: comment})
	}
	return nil
}

func (s *state) dropTextSearchConfig(drop *schema.DropObject, c *TextSearchConfig) error {
	reverse, err := s.textSearchCreate(c)
	if err != nil {
		return err
	}
	b := s.Build("DROP TEXT SEARCH CONFIGURATION")
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	b.P(s.textSearchIdent(c))
	if sqlx.Has(drop.Extra, &Cascade{}) {
		b.P("CASCADE")
	}
	s.append(&migrate.Change{Source: drop, Cmd: b.String(), Reverse: reverse, Comment: fmt.Sprintf("drop text search configuration %q", c.Name)})
	return nil
}

func (s *state) modifyTextSearchConfig(change *schema.ModifyObject, from, to *TextSearchConfig) error {
	if err := validateTextSearchConfig(from); err != nil {
		return err
	}
	if err := validateTextSearchConfig(to); err != nil {
		return err
	}
	p1, _ := textSearchRef(from.Parser)
	p2, _ := textSearchRef(to.Parser)
	// PostgreSQL cannot alter the parser without recreating the configuration.
	if p1 != p2 {
		return fmt.Errorf("postgres: changing the parser of text search configuration %q requires dropping and recreating it", from.Name)
	}
	m1, m2 := textSearchMappings(from), textSearchMappings(to)
	var tokens []string
	for t := range m1 {
		tokens = append(tokens, t)
	}
	for t := range m2 {
		if _, ok := m1[t]; !ok {
			tokens = append(tokens, t)
		}
	}
	sort.Strings(tokens)
	for _, t := range tokens {
		if reflect.DeepEqual(m1[t], m2[t]) {
			continue
		}
		s.append(&migrate.Change{Source: change, Cmd: s.textSearchMapping(to, t, m2[t]), Reverse: s.textSearchMapping(from, t, m1[t]), Comment: fmt.Sprintf("modify text search configuration %q mapping %q", to.Name, t)})
	}
	if c1, c2 := textSearchComment(from), textSearchComment(to); c1 != c2 {
		s.append(&migrate.Change{Source: change, Cmd: s.textSearchComment(to, c2), Reverse: s.textSearchComment(from, c1), Comment: fmt.Sprintf("modify text search configuration %q comment", to.Name)})
	}
	return nil
}
