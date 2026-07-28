package releasepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type goreleaserConfig struct {
	Release struct {
		Draft bool `yaml:"draft"`
	} `yaml:"release"`
	Brews []struct {
		SkipUpload bool `yaml:"skip_upload"`
	} `yaml:"brews"`
}

type workflowConfig struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Needs       string            `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

func TestReleaseRemainsDraftUntilAttested(t *testing.T) {
	goreleaser := loadYAML[goreleaserConfig](t, ".goreleaser.yml")
	if !goreleaser.Release.Draft {
		t.Fatal("GoReleaser publishes assets before attestation; release.draft must be true")
	}
	if len(goreleaser.Brews) == 0 || !goreleaser.Brews[0].SkipUpload {
		t.Fatal("GoReleaser publishes the Homebrew formula before attestation; brews.skip_upload must be true")
	}

	workflow := loadYAML[workflowConfig](t, ".github/workflows/release.yml")
	attest, ok := workflow.Jobs["attest"]
	if !ok {
		t.Fatal("release workflow has no attest job")
	}
	if attest.Needs != "goreleaser" {
		t.Fatalf("attest job must need goreleaser, got %q", attest.Needs)
	}
	if attest.Permissions["contents"] != "write" {
		t.Fatal("attest job needs contents: write to publish the draft")
	}

	attestIndex, publishIndex, homebrewIndex := -1, -1, -1
	formulaPreserved := false
	var publish, homebrew workflowStep
	for jobName, job := range workflow.Jobs {
		for index, step := range job.Steps {
			if jobName == "goreleaser" && strings.Contains(step.With["path"], "dist/homebrew/Formula/vibeusage.rb") {
				formulaPreserved = true
			}
			if jobName == "attest" && strings.HasPrefix(step.Uses, "actions/attest-build-provenance@") {
				attestIndex = index
			}
			if strings.Contains(step.Run, "gh release edit") && strings.Contains(step.Run, "--draft=false") {
				if jobName != "attest" || publishIndex >= 0 {
					t.Fatal("release must be published exactly once in the attest job")
				}
				publishIndex = index
				publish = step
			}
			if strings.Contains(step.Run, "homebrew-homebrew/contents/Formula/vibeusage.rb") {
				if jobName != "attest" || homebrewIndex >= 0 {
					t.Fatal("Homebrew formula must be published exactly once in the attest job")
				}
				homebrewIndex = index
				homebrew = step
			}
		}
	}

	if attestIndex < 0 {
		t.Fatal("attest job has no build-provenance step")
	}
	if publishIndex <= attestIndex {
		t.Fatal("draft release must be published after build provenance is attested")
	}
	if publish.If != "" {
		t.Fatal("publish step must retain the default success condition")
	}
	const command = `gh release edit "$GITHUB_REF_NAME" --draft=false --repo "$GITHUB_REPOSITORY"`
	if strings.TrimSpace(publish.Run) != command {
		t.Fatalf("unexpected release publish command: %q", publish.Run)
	}
	if publish.Env["GH_TOKEN"] != "${{ github.token }}" {
		t.Fatal("publish step must authenticate gh with github.token")
	}
	if !formulaPreserved {
		t.Fatal("release artifacts must preserve the generated Homebrew formula")
	}
	if homebrewIndex <= publishIndex {
		t.Fatal("Homebrew formula must be published after the attested GitHub release")
	}
	if homebrew.If != "" {
		t.Fatal("Homebrew publish step must retain the default success condition")
	}
	if homebrew.Env["GH_TOKEN"] != "${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}" {
		t.Fatal("Homebrew publish step must use the tap token")
	}
	if !strings.Contains(homebrew.Run, `gh api --method GET "$endpoint"`) {
		t.Fatal("Homebrew publish step must fetch the current formula SHA with GET")
	}
}

func loadYAML[T any](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var value T
	if err := yaml.Unmarshal(data, &value); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return value
}
