// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package integration

import (
	"context"
	"strconv"
	"testing"

	"ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"
	"github.com/stretchr/testify/require"
)

func TestPostgres_TextSearchConfig(t *testing.T) {
	pgRun(t, func(t *pgTest) {
		ctx := context.Background()
		s := textSearchSchema(t)
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

		// Inspect configurations without tables.
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

		// Recreate the schema from HCL.
		_, err = t.db.ExecContext(ctx, `DROP TABLE textsearch_test.cars`)
		require.NoError(t, err)
		for _, name := range []string{`usimple`, `empty`, `"quote""config"`} {
			_, err = t.db.ExecContext(ctx, `DROP TEXT SEARCH CONFIGURATION textsearch_test.`+name)
			require.NoError(t, err)
		}
		changes, err = t.drv.SchemaDiff(schema.New("textsearch_test"), &desired)
		require.NoError(t, err)
		require.NoError(t, t.drv.ApplyChanges(ctx, changes))
		s, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
		require.NoError(t, err)
		changes, err = t.drv.SchemaDiff(s, &desired)
		require.NoError(t, err)
		require.Empty(t, changes)
		_, err = t.db.ExecContext(ctx, `INSERT INTO textsearch_test.cars(name) VALUES ('Hôtel')`)
		require.NoError(t, err)
		var matches bool
		require.NoError(t, t.db.QueryRow(`SELECT ts @@ to_tsquery('textsearch_test.usimple', 'hotel') FROM textsearch_test.cars`).Scan(&matches))
		require.True(t, matches)

		// Drops must remove the referencing table first, even if objects come first.
		var drops []schema.Change
		for _, o := range s.Objects {
			drops = append(drops, &schema.DropObject{O: o})
		}
		drops = append(drops, &schema.DropTable{T: s.Tables[0]})
		require.NoError(t, t.drv.ApplyChanges(ctx, drops))
		current, err := t.drv.InspectSchema(ctx, "textsearch_test", nil)
		require.NoError(t, err)
		require.Empty(t, current.Objects)
		require.Empty(t, current.Tables)
	})
}

func TestPostgres_TextSearchConfig_Modify(t *testing.T) {
	pgRun(t, func(t *pgTest) {
		ctx := context.Background()
		s := textSearchSchema(t)
		buf, err := postgres.MarshalHCL(s)
		require.NoError(t, err)
		var desired schema.Schema
		require.NoError(t, postgres.EvalHCLBytes(buf, &desired, nil))

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
		changes, err := t.drv.SchemaDiff(s, &desired)
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
				_, err = t.db.ExecContext(ctx, stmt)
				require.NoError(t, err)
			}
		}
		current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
		require.NoError(t, err)
		changes, err = t.drv.SchemaDiff(current, s)
		require.NoError(t, err)
		require.Empty(t, changes)
	})
}

func TestPostgres_TextSearchConfig_Normalize(t *testing.T) {
	pgRun(t, func(t *pgTest) {
		ctx := context.Background()
		s := textSearchSchema(t)
		buf, err := postgres.MarshalHCL(s)
		require.NoError(t, err)
		changes, err := t.drv.SchemaDiff(s, schema.New(s.Name))
		require.NoError(t, err)
		require.NoError(t, t.drv.ApplyChanges(ctx, changes))

		dev, err := sqlclient.Open(ctx, t.url("textsearch_test"))
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
		current, err := t.drv.InspectSchema(ctx, "textsearch_test", nil)
		require.NoError(t, err)
		require.Empty(t, current.Objects)
		require.Empty(t, current.Tables)

		var schemaInput schema.Schema
		require.NoError(t, postgres.EvalHCLBytes(buf, &schemaInput, nil))
		normalizedSchema, err := dev.Driver.(schema.Normalizer).NormalizeSchema(ctx, &schemaInput)
		require.NoError(t, err)
		require.Len(t, normalizedSchema.Tables, 1)
		from := schema.New(s.Name).AddObjects(s.Objects...)
		to := schema.New(normalizedSchema.Name).AddObjects(normalizedSchema.Objects...)
		changes, err = t.drv.SchemaDiff(from, to)
		require.NoError(t, err)
		require.Empty(t, changes)
		current, err = t.drv.InspectSchema(ctx, "textsearch_test", nil)
		require.NoError(t, err)
		require.Empty(t, current.Objects)
		require.Empty(t, current.Tables)
	})
}

func textSearchSchema(t *pgTest) *schema.Schema {
	t.Helper()
	ctx := context.Background()
	exec := func(stmt string) {
		_, err := t.db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	exec(`CREATE SCHEMA textsearch_test`)
	t.Cleanup(func() {
		_, err := t.db.Exec(`DROP SCHEMA IF EXISTS textsearch_test CASCADE`)
		require.NoError(t, err)
	})
	exec(`CREATE EXTENSION IF NOT EXISTS unaccent`)
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test.usimple (COPY = pg_catalog.simple)`)
	exec(`ALTER TEXT SEARCH CONFIGURATION textsearch_test.usimple ALTER MAPPING FOR hword, hword_part, word WITH public.unaccent, pg_catalog.simple`)
	exec(`COMMENT ON TEXT SEARCH CONFIGURATION textsearch_test.usimple IS 'Unaccented search'`)
	version, err := strconv.Atoi(t.drv.(*postgres.Driver).Version())
	require.NoError(t, err)
	if version >= 12_00_00 {
		exec(`CREATE TABLE textsearch_test.cars (name text, ts tsvector GENERATED ALWAYS AS (to_tsvector('textsearch_test.usimple'::regconfig, name)) STORED)`)
	} else {
		// Generated columns require PostgreSQL 12 or later.
		exec(`CREATE TABLE textsearch_test.cars (name text, ts tsvector DEFAULT to_tsvector('textsearch_test.usimple'::regconfig, 'Hôtel'))`)
	}
	exec(`CREATE INDEX cars_ts ON textsearch_test.cars USING gin(ts)`)
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test.empty (PARSER = pg_catalog.default)`)
	exec(`CREATE TEXT SEARCH CONFIGURATION textsearch_test."quote""config" (PARSER = pg_catalog.default)`)
	s, err := t.drv.InspectSchema(ctx, "textsearch_test", nil)
	require.NoError(t, err)
	require.Len(t, s.Objects, 3)
	return s
}
