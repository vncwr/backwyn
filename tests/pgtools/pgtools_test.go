package pgtools_test

import (
	"reflect"
	"testing"

	"github.com/vncwr/backwyn/internal/pgtools"
)

func TestDumpArgs(t *testing.T) {
	cases := []struct {
		name    string
		schemas []string
		want    []string
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pgtools.DumpArgs("postgresql://x", "/tmp/out.pgc", tc.schemas)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DumpArgs = %v, want %v", got, tc.want)
			}
		})
	}
}
