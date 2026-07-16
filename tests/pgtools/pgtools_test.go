package pgtools_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vncwr/backwyn/internal/pgtools"
)

func TestFilterSandboxTOC(t *testing.T) {
	toc := `;
; Archive created at 2026-07-16 04:30:00 UTC
;
217; 1247 25534 SCHEMA - public pg_database_owner
218; 1259 24597 TABLE public customers postgres
3625; 0 0 POLICY public customers backwyn_read
3626; 0 0 POLICY public customers tenant_isolation
219; 0 24597 TABLE DATA public customers postgres
220; 1259 24601 SCHEMA - app postgres`

	got := pgtools.FilterSandboxTOC(toc)
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.Contains(line, "POLICY"), strings.Contains(line, "SCHEMA - public"):
			if !strings.HasPrefix(line, ";") {
				t.Errorf("sandbox-unsatisfiable entry not commented out: %q", line)
			}
		case strings.Contains(line, "TABLE"), strings.Contains(line, "SCHEMA - app"):
			if strings.HasPrefix(line, ";") {
				t.Errorf("data entry wrongly commented out: %q", line)
			}
		}
	}
}

func TestDumpArgs(t *testing.T) {
	cases := []struct {
		name        string
		schemas     []string
		rowSecurity bool
		want        []string
	}{
		{
			name:    "no schemas dumps the whole database",
			schemas: nil,
			want: []string{
				"--format=custom", "--no-owner", "--no-privileges",
				"--file", "/tmp/out.pgc", "--dbname", "postgresql://x",
			},
		},
		{
			name:    "schemas become -n flags in order",
			schemas: []string{"public", "app"},
			want: []string{
				"--format=custom", "--no-owner", "--no-privileges",
				"-n", "public", "-n", "app",
				"--file", "/tmp/out.pgc", "--dbname", "postgresql://x",
			},
		},
		{
			name:    "blank entries are dropped",
			schemas: []string{" public ", "", "  "},
			want: []string{
				"--format=custom", "--no-owner", "--no-privileges",
				"-n", "public",
				"--file", "/tmp/out.pgc", "--dbname", "postgresql://x",
			},
		},
		{
			name:        "row security adds --enable-row-security",
			schemas:     []string{"public"},
			rowSecurity: true,
			want: []string{
				"--format=custom", "--no-owner", "--no-privileges",
				"--enable-row-security",
				"-n", "public",
				"--file", "/tmp/out.pgc", "--dbname", "postgresql://x",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pgtools.DumpArgs("postgresql://x", "/tmp/out.pgc", tc.schemas, tc.rowSecurity)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DumpArgs = %v, want %v", got, tc.want)
			}
		})
	}
}
