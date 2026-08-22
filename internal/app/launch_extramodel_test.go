package app

import (
	"reflect"
	"testing"
)

func TestExtractModelFromExtraArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantModel   string
		wantCleaned []string
	}{
		{
			name:        "space form with other flags",
			args:        []string{"--dangerously-skip-permissions", "--model", "x-preview-f"},
			wantModel:   "x-preview-f",
			wantCleaned: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "equals form",
			args:        []string{"--dangerously-skip-permissions", "--model=x-preview-f"},
			wantModel:   "x-preview-f",
			wantCleaned: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "model first then other flags",
			args:        []string{"--model", "glm-5.2", "--dangerously-skip-permissions"},
			wantModel:   "glm-5.2",
			wantCleaned: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "no model flag",
			args:        []string{"--dangerously-skip-permissions", "-p", "hello"},
			wantModel:   "",
			wantCleaned: []string{"--dangerously-skip-permissions", "-p", "hello"},
		},
		{
			name:        "model at end with no value",
			args:        []string{"--dangerously-skip-permissions", "--model"},
			wantModel:   "",
			wantCleaned: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "single dash form",
			args:        []string{"-model", "x-preview-f", "-p", "hi"},
			wantModel:   "x-preview-f",
			wantCleaned: []string{"-p", "hi"},
		},
		{
			name:        "single dash equals form",
			args:        []string{"-model=deepseek-v4-flash", "--dangerously-skip-permissions"},
			wantModel:   "deepseek-v4-flash",
			wantCleaned: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "empty args",
			args:        []string{},
			wantModel:   "",
			wantCleaned: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model, cleaned := extractModelFromExtraArgs(tc.args)
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if !reflect.DeepEqual(cleaned, tc.wantCleaned) {
				t.Errorf("cleaned = %v, want %v", cleaned, tc.wantCleaned)
			}
		})
	}
}
