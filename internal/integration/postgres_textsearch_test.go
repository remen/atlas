// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package integration

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"testing"

	"ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"
	"github.com/stretchr/testify/require"
)

func TestPostgres_TextSearchConfig(t *testing.T) {
	// Allow a dedicated server without binding any of the standard harness ports.
	if url := os.Getenv("ATLAS_POSTGRES_TEST_URL"); url != "" {
		db, err := sql.Open("postgres", url)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		drv, err := postgres.Open(db)
		require.NoError(t, err)
		testPostgresTextSearchConfig(&pgTest{T: t, db: db, drv: drv}, url)
		return
	}
	pgRun(t, func(t *pgTest) { testPostgresTextSearchConfig(t, t.url("")) })
}

func testPostgresTextSearchConfig(t *pgTest, databaseURL string) {
	ctx := context.Background()
	exec := func(stmt string) { _, err := t.db.ExecContext(ctx, stmt); require.NoError(t, err) }
	exec(`CREATE SCHEMA textsearch_test`)
	t.Cleanup(func() { _, err := t.db.Exec(`DROP SCHEMA IF EXISTS textsearch_test CASCADE`); require.NoError(t, err) })
	exec(`CREATE EXTENSION IF NOT EXISTS unaccent`)
	// Reproduce issue #2153, including COPY, the ordered dictionary chain,
	// and a generated tsvector column that depends on the configuration.
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test.usimple (COPY = pg_catalog.simple)`)
	exec(`ALTER TEXT SEARCH CONFIGURATION textsearch_test.usimple ALTER MAPPING FOR hword, hword_part, word WITH public.unaccent, pg_catalog.simple`)
	exec(`COMMENT ON TEXT SEARCH CONFIGURATION textsearch_test.usimple IS 'Unaccented search'`)
	version, err := strconv.Atoi(t.drv.(*postgres.Driver).Version())
	require.NoError(t, err)
	if version >= 12_00_00 {
		exec(`CREATE TABLE textsearch_test.cars (name text, ts tsvector GENERATED ALWAYS AS (to_tsvector('textsearch_test.usimple'::regconfig, name)) STORED)`)
	} else {
		// PostgreSQL 10/11 do not support generated columns. A default expression
		// still exercises the configuration dependency on these versions.
		exec(`CREATE TABLE textsearch_test.cars (name text, ts tsvector DEFAULT to_tsvector('textsearch_test.usimple'::regconfig, 'Hôtel'))`)
	}
	exec(`CREATE INDEX cars_ts ON textsearch_test.cars USING gin(ts)`)
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test.empty (PARSER = pg_catalog.default)`)
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test."quote""config" (PARSER = pg_catalog.default)`)
	s, err := t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	require.Len(t, s.Objects, 3)
	var c *postgres.TextSearchConfig
	for _, o := range s.Objects {
		if x := o.(*postgres.TextSearchConfig); x.Name == "usimple" {
			c = x
		}
	}
	require.NotNil(t, c)
	require.Equal(t, `pg_catalog."default"`, c.Parser)
	require.NotEmpty(t, c.Mappings)
	var word *postgres.TextSearchMapping
	for _, m := range c.Mappings {
		if m.Token == "word" {
			word = m
		}
	}
	require.NotNil(t, word)
	require.Equal(t, []string{"public.unaccent", "pg_catalog.simple"}, word.Dictionaries)
	// InspectObjects is independent of table/type inspection.
	objects, err := t.drv.InspectSchema(ctx, "textsearch_test", &schema.InspectOptions{Mode: schema.InspectObjects})
	require.NoError(t, err)
	require.Len(t, objects.Objects, 3)
	require.Empty(t, objects.Tables)
	realm, err := t.drv.InspectRealm(ctx, &schema.InspectRealmOption{Schemas: []string{"textsearch_test"}, Mode: schema.InspectObjects})
	require.NoError(t, err)
	require.Len(t, realm.Schemas[0].Objects, 3)
	buf, err := postgres.MarshalHCL(s)
	require.NoError(t, err)
	var desired schema.Schema
	require.NoError(t, postgres.EvalHCLBytes(buf, &desired, nil))
	changes, err := t.drv.SchemaDiff(s, &desired)
	require.NoError(t, err)
	require.Empty(t, changes)
	// Recreate from HCL, with configuration additions deliberately after tables.
	exec(`DROP TABLE textsearch_test.cars`)
	for _, name := range []string{`usimple`, `empty`, `"quote""config"`} {
		exec(`DROP TEXT SEARCH CONFIGURATION textsearch_test.` + name)
	}
	changes, err = t.drv.SchemaDiff(schema.New("textsearch_test"), &desired)
	require.NoError(t, err)
	require.NoError(t, t.drv.ApplyChanges(ctx, changes))
	s, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	changes, err = t.drv.SchemaDiff(s, &desired)
	require.NoError(t, err)
	require.Empty(t, changes)
	exec(`INSERT INTO textsearch_test.cars(name) VALUES ('Hôtel')`)
	var matches bool
	require.NoError(t, t.db.QueryRow(`SELECT ts @@ to_tsquery('textsearch_test.usimple', 'hotel') FROM textsearch_test.cars`).Scan(&matches))
	require.True(t, matches)
	// Add/change/remove mappings and comments, then execute reverse statements.
	var dc *postgres.TextSearchConfig
	for _, o := range desired.Objects {
		if x := o.(*postgres.TextSearchConfig); x.Name == "usimple" {
			dc = x
		}
	}
	var mappings []*postgres.TextSearchMapping
	for _, m := range dc.Mappings {
		switch m.Token {
		case "asciiword":
			continue
		case "word":
			mappings = append(mappings, &postgres.TextSearchMapping{Token: m.Token, Dictionaries: []string{"pg_catalog.simple"}})
		default:
			mappings = append(mappings, m)
		}
	}
	// 'blank' has no mapping in the simple configuration.
	mappings = append(mappings, &postgres.TextSearchMapping{Token: "blank", Dictionaries: []string{"pg_catalog.simple"}})
	dc.Mappings = mappings
	dc.Attrs = []schema.Attr{&schema.Comment{Text: "updated 'comment'"}}
	changes, err = t.drv.SchemaDiff(s, &desired)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	plan, err := t.drv.PlanChanges(ctx, "modify", changes)
	require.NoError(t, err)
	require.True(t, plan.Reversible)
	require.NoError(t, t.drv.ApplyChanges(ctx, changes))
	current, err := t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	changes, err = t.drv.SchemaDiff(current, &desired)
	require.NoError(t, err)
	require.Empty(t, changes)
	for i := len(plan.Changes) - 1; i >= 0; i-- {
		stmts, err := plan.Changes[i].ReverseStmts()
		require.NoError(t, err)
		for _, stmt := range stmts {
			exec(stmt)
		}
	}
	current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	changes, err = t.drv.SchemaDiff(current, s)
	require.NoError(t, err)
	require.Empty(t, changes)
	// Drops must remove the referencing table first, even if objects come first.
	var drops []schema.Change
	for _, o := range current.Objects {
		drops = append(drops, &schema.DropObject{O: o})
	}
	drops = append(drops, &schema.DropTable{T: current.Tables[0]})
	require.NoError(t, t.drv.ApplyChanges(ctx, drops))
	current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	require.Empty(t, current.Objects)
	require.Empty(t, current.Tables)
	// NormalizeRealm must preserve objects and restore the development database.
	u, err := url.Parse(databaseURL)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", "textsearch_test")
	u.RawQuery = q.Encode()
	dev, err := sqlclient.Open(ctx, u.String())
	require.NoError(t, err)
	defer dev.Close()
	var normalizedInput schema.Realm
	require.NoError(t, postgres.EvalHCLBytes(buf, &normalizedInput, nil))
	normalized, err := dev.Driver.(schema.Normalizer).NormalizeRealm(ctx, &normalizedInput)
	require.NoError(t, err)
	require.Len(t, normalized.Schemas, 1)
	require.Len(t, normalized.Schemas[0].Objects, 3)
	require.Len(t, normalized.Schemas[0].Tables, 1)
	changes, err = t.drv.SchemaDiff(s, normalized.Schemas[0])
	require.NoError(t, err)
	require.Empty(t, changes)
	current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	require.Empty(t, current.Objects)
	require.Empty(t, current.Tables)
	var schemaInput schema.Schema
	require.NoError(t, postgres.EvalHCLBytes(buf, &schemaInput, nil))
	// Start with schema references that are distinct from the root schema.
	// NormalizeSchema patches object references just as it does table references.
	normalizedSchema, err := dev.Driver.(schema.Normalizer).NormalizeSchema(ctx, &schemaInput)
	require.NoError(t, err)
	require.Len(t, normalizedSchema.Tables, 1)
	changes, err = t.drv.SchemaDiff(schema.New(s.Name).AddObjects(s.Objects...), schema.New(normalizedSchema.Name).AddObjects(normalizedSchema.Objects...))
	require.NoError(t, err)
	require.Empty(t, changes)
	current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	require.Empty(t, current.Objects)
	require.Empty(t, current.Tables)
}
