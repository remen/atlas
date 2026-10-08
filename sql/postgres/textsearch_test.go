// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package postgres

import (
	"context"
	"fmt"
	"testing"

	"ariga.io/atlas/sql/internal/sqltest"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestTextSearchConfig_HCL(t *testing.T) {
	input := `schema "public" {}
text_search_config "usimple" {
  schema  = schema.public
  parser  = "pg_catalog.default"
  comment = "Unaccented search"
  mapping "word" {
    dictionaries = ["public.unaccent", "pg_catalog.simple"]
  }
  mapping "asciiword" {
    dictionaries = ["pg_catalog.simple"]
  }
}`
	var s schema.Schema
	require.NoError(t, EvalHCLBytes([]byte(input), &s, nil))
	require.Len(t, s.Objects, 1)
	c := s.Objects[0].(*TextSearchConfig)
	require.Equal(t, "usimple", c.Name)
	require.Equal(t, "pg_catalog.default", c.Parser)
	require.Equal(t, []string{"public.unaccent", "pg_catalog.simple"}, c.Mappings[0].Dictionaries)
	buf, err := MarshalHCL(&s)
	require.NoError(t, err)
	var s2 schema.Schema
	require.NoError(t, EvalHCLBytes(buf, &s2, nil))
	changes, err := DefaultDiff.SchemaDiff(&s, &s2)
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestTextSearchConfig_HCL_Qualified(t *testing.T) {
	s1, s2 := schema.New("public"), schema.New("other")
	for _, s := range []*schema.Schema{s1, s2} {
		s.AddObjects(&TextSearchConfig{
			Name:   "usimple",
			Schema: s,
			Parser: "pg_catalog.default",
		})
	}
	buf, err := MarshalHCL(schema.NewRealm(s1, s2))
	require.NoError(t, err)
	var r schema.Realm
	require.NoError(t, EvalHCLBytes(buf, &r, nil))
	require.Len(t, r.Schemas, 2)
	for _, s := range r.Schemas {
		require.Len(t, s.Objects, 1)
	}
	public, ok := r.Schema("public")
	require.True(t, ok)
	excluded, err := schema.ExcludeSchema(public, []string{"usimple[type=text_search_config]"})
	require.NoError(t, err)
	require.Empty(t, excluded.Objects)
}

func TestTextSearchConfig_Plan(t *testing.T) {
	s := schema.New("public")
	c := &TextSearchConfig{
		Name:   "usimple",
		Schema: s,
		Parser: "pg_catalog.default",
		Mappings: []*TextSearchMapping{
			{Token: "word", Dictionaries: []string{"public.unaccent", "pg_catalog.simple"}},
		},
	}
	table := schema.NewTable("cars").SetSchema(s).AddColumns(schema.NewStringColumn("name", "text"))
	plan, err := DefaultPlan.PlanChanges(context.Background(), "create", []schema.Change{
		&schema.AddTable{T: table},
		&schema.AddObject{O: c},
	})
	require.NoError(t, err)
	require.True(t, plan.Reversible)
	require.Equal(t, `CREATE TEXT SEARCH CONFIGURATION "public"."usimple" (PARSER = "pg_catalog"."default")`, plan.Changes[0].Cmd)
	require.Equal(t, `ALTER TEXT SEARCH CONFIGURATION "public"."usimple" ALTER MAPPING FOR "word" WITH "public"."unaccent", "pg_catalog"."simple"`, plan.Changes[1].Cmd)
	require.Contains(t, plan.Changes[2].Cmd, "CREATE TABLE")

	plan, err = DefaultPlan.PlanChanges(context.Background(), "drop", []schema.Change{
		&schema.DropObject{O: c, Extra: []schema.Clause{&schema.IfExists{}, &Cascade{}}},
		&schema.DropTable{T: table},
	})
	require.NoError(t, err)
	require.Contains(t, plan.Changes[0].Cmd, "DROP TABLE")
	require.Equal(t, `DROP TEXT SEARCH CONFIGURATION IF EXISTS "public"."usimple" CASCADE`, plan.Changes[1].Cmd)
	reverse, err := plan.Changes[1].ReverseStmts()
	require.NoError(t, err)
	require.Len(t, reverse, 2)

	plan, err = DefaultPlan.PlanChanges(context.Background(), "qualified", []schema.Change{
		&schema.AddObject{O: c},
	}, func(o *migrate.PlanOptions) {
		q := "other"
		o.SchemaQualifier = &q
	})
	require.NoError(t, err)
	require.Equal(t, `CREATE TEXT SEARCH CONFIGURATION "other"."usimple" (PARSER = "pg_catalog"."default")`, plan.Changes[0].Cmd)
}

func TestTextSearchConfig_Diff(t *testing.T) {
	from, to := schema.New("public"), schema.New("public")
	c1 := &TextSearchConfig{
		Name:   "search",
		Schema: from,
		Parser: "pg_catalog.default",
		Mappings: []*TextSearchMapping{
			{Token: "word", Dictionaries: []string{"public.unaccent", "pg_catalog.simple"}},
			{Token: "asciiword", Dictionaries: []string{"pg_catalog.simple"}},
		},
	}
	c2 := &TextSearchConfig{
		Name:   "search",
		Schema: to,
		Parser: `"pg_catalog"."default"`,
		Mappings: []*TextSearchMapping{
			{Token: "asciiword", Dictionaries: []string{`"pg_catalog"."simple"`}},
			{Token: "word", Dictionaries: []string{`"public"."unaccent"`, `"pg_catalog"."simple"`}},
		},
	}
	from.AddObjects(c1)
	to.AddObjects(c2)
	changes, err := DefaultDiff.SchemaDiff(from, to)
	require.NoError(t, err)
	require.Empty(t, changes)

	// Dictionary order is significant; mapping order is not.
	c2.Mappings[1].Dictionaries = []string{"pg_catalog.simple", "public.unaccent"}
	c2.Mappings[0].Token = "hword"
	c2.Attrs = []schema.Attr{&schema.Comment{Text: "a 'quoted' comment"}}
	changes, err = DefaultDiff.SchemaDiff(from, to)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	plan, err := DefaultPlan.PlanChanges(context.Background(), "modify", changes)
	require.NoError(t, err)
	require.True(t, plan.Reversible)
	require.Len(t, plan.Changes, 4)
	require.Contains(t, plan.Changes[0].Cmd, `DROP MAPPING FOR "asciiword"`)
	require.Contains(t, plan.Changes[1].Cmd, `ALTER MAPPING FOR "hword"`)
	require.Contains(t, plan.Changes[2].Cmd, `WITH "pg_catalog"."simple", "public"."unaccent"`)
	require.Contains(t, plan.Changes[3].Cmd, "'a ''quoted'' comment'")

	changes, err = DefaultDiff.SchemaDiff(from, schema.New("public"))
	require.NoError(t, err)
	require.IsType(t, &schema.DropObject{}, changes[0])
	changes, err = DefaultDiff.SchemaDiff(schema.New("public"), to)
	require.NoError(t, err)
	require.IsType(t, &schema.AddObject{}, changes[0])
}

func TestTextSearchConfig_Validation(t *testing.T) {
	for _, input := range []string{"", "parser;DROP TABLE cars", "a.b.c", `"unclosed`, "a..b"} {
		_, err := textSearchRef(input)
		require.Error(t, err)
	}
	ref, err := textSearchRef(`"odd.schema"."quote""name"`)
	require.NoError(t, err)
	require.Equal(t, `"odd.schema"."quote""name"`, ref)
	tests := []struct {
		name     string
		mappings []*TextSearchMapping
	}{
		{
			name:     "nil mapping",
			mappings: []*TextSearchMapping{nil},
		},
		{
			name:     "no dictionaries",
			mappings: []*TextSearchMapping{{Token: "word"}},
		},
		{
			name: "duplicate token",
			mappings: []*TextSearchMapping{
				{Token: "word", Dictionaries: []string{"pg_catalog.simple"}},
				{Token: "word", Dictionaries: []string{"pg_catalog.simple"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &TextSearchConfig{
				Name:     "search",
				Parser:   "pg_catalog.default",
				Mappings: tt.mappings,
			}
			_, err := DefaultPlan.PlanChanges(context.Background(), "invalid", []schema.Change{
				&schema.AddObject{O: c},
			})
			require.Error(t, err)
			_, err = MarshalHCL(schema.New("public").AddObjects(c))
			require.Error(t, err)
		})
	}

	from := &TextSearchConfig{Name: "search", Parser: "pg_catalog.default"}
	to := &TextSearchConfig{Name: "search", Parser: "public.other"}
	_, err = DefaultPlan.PlanChanges(context.Background(), "parser", []schema.Change{
		&schema.ModifyObject{From: from, To: to},
	})
	require.ErrorContains(t, err, "requires dropping and recreating")
}

func TestTextSearchConfig_Inspect(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	s := schema.New("public")
	mock.ExpectQuery(sqltest.Escape(fmt.Sprintf(textSearchConfigsQuery, "$1"))).WithArgs("public").WillReturnRows(
		sqlmock.NewRows([]string{"id", "schema", "name", "parser", "comment", "token", "dictionary"}).
			AddRow(1, "public", "search", "pg_catalog.default", "comment", "word", "public.unaccent").
			AddRow(1, "public", "search", "pg_catalog.default", "comment", "word", "pg_catalog.simple").
			AddRow(2, "public", "empty", "pg_catalog.default", nil, nil, nil),
	)
	require.NoError(t, (&inspect{conn: &conn{ExecQuerier: db}}).inspectTextSearchConfigs(context.Background(), schema.NewRealm(s)))
	require.Len(t, s.Objects, 2)
	c := s.Objects[0].(*TextSearchConfig)
	require.Equal(t, []string{"public.unaccent", "pg_catalog.simple"}, c.Mappings[0].Dictionaries)
	require.Equal(t, "comment", textSearchComment(c))
	require.Empty(t, s.Objects[1].(*TextSearchConfig).Mappings)
	require.NoError(t, mock.ExpectationsWereMet())
}
