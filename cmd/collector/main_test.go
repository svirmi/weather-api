package main

import (
	"os"
	"testing"
)

func TestSubcommandArgs(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()

	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"/collector"}, nil},
		{[]string{"/collector", "run"}, nil},
		{[]string{"/collector", "run", "--flag"}, []string{"--flag"}},
	}
	for _, tt := range tests {
		os.Args = tt.args
		got := subcommandArgs()
		if len(got) != len(tt.want) {
			t.Fatalf("args=%v: got %v, want %v", tt.args, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("args=%v: got %v, want %v", tt.args, got, tt.want)
			}
		}
	}
}
